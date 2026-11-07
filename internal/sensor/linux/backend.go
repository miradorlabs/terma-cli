//go:build linux

package linux

import (
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"

	"github.com/miradorlabs/terma-cli/internal/sensor"
)

// Backend runs the sensor's eBPF programs. Closing it, or the process exiting, detaches
// every program: nothing is pinned, so a sensor that dies leaves the kernel as it found it
// and denies nothing.
type Backend struct {
	objs  sensorObjects
	links []link.Link
	boot  time.Time // wall time at boot, to place the programs' boot-clock timestamps

	mu    sync.Mutex
	roots map[int]bool
}

// New loads and attaches the programs. It needs root (CAP_BPF, CAP_PERFMON, CAP_SYS_ADMIN
// for the LSM hooks) and a kernel with BTF and, for enforcement, the bpf LSM.
func New() (*Backend, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, err
	}
	b := &Backend{roots: map[int]bool{}}
	if err := loadSensorObjects(&b.objs, nil); err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			return nil, fmt.Errorf("verifier: %w\n%s", err, strings.Join(ve.Log, "\n"))
		}
		return nil, err
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return nil, err
	}
	b.boot = time.Now().Add(-time.Duration(ts.Nano()))

	p := b.objs.sensorPrograms
	tracing := map[string]*ebpf.Program{
		"on_exec": p.OnExec, "on_fork": p.OnFork, "on_exit": p.OnExit,
		"tcp_send": p.TcpSend, "tcp_recv": p.TcpRecv, "udp_send": p.UdpSend, "udp6_send": p.Udp6Send,
		"udp_recv": p.UdpRecv, "udp6_recv": p.Udp6Recv, "sock_release": p.SockRelease,
		"file_open": p.FileOpen, "path_unlink": p.PathUnlink, "path_rename": p.PathRename,
	}
	for name, prog := range tracing {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
			b.Close()
			return nil, fmt.Errorf("attach %s: %w", name, err)
		}
		b.links = append(b.links, l)
	}
	for name, prog := range map[string]*ebpf.Program{"sys_enter": p.SysEnter, "sys_exit": p.SysExit} {
		l, err := link.Tracepoint("raw_syscalls", name, prog, nil)
		if err != nil {
			b.Close()
			return nil, fmt.Errorf("attach %s: %w", name, err)
		}
		b.links = append(b.links, l)
	}
	// DNS answers, read on their way in to any socket in any cgroup: a host rule denies the
	// addresses before the program that asked can use them.
	dns, err := link.AttachCgroup(link.CgroupOptions{Path: "/sys/fs/cgroup", Attach: ebpf.AttachCGroupInetIngress, Program: p.DnsIngress})
	if err != nil {
		b.Close()
		return nil, fmt.Errorf("attach dns_ingress: %w", err)
	}
	b.links = append(b.links, dns)
	for id, dir := range rwSyscalls() {
		if err := b.objs.RwSyscalls.Update(&id, &dir, ebpf.UpdateAny); err != nil {
			b.Close()
			return nil, err
		}
	}
	lsm := map[string]*ebpf.Program{"deny_connect": p.DenyConnect, "deny_open": p.DenyOpen, "file_release": p.FileRelease}
	for name, prog := range lsm {
		l, err := link.AttachLSM(link.LSMOptions{Program: prog})
		if err != nil {
			b.Close()
			return nil, fmt.Errorf("attach %s (is the bpf LSM enabled?): %w", name, err)
		}
		b.links = append(b.links, l)
	}
	return b, nil
}

// Close detaches every program and frees the maps.
func (b *Backend) Close() {
	for _, l := range b.links {
		_ = l.Close()
	}
	_ = b.objs.Close()
}

func (b *Backend) at(boot uint64) time.Time { return b.boot.Add(time.Duration(boot)) }

// Roots has the kernel start a session at every exec of one of exes (by base name).
func (b *Backend) Roots(exes []string) error {
	for _, e := range exes {
		var k sensorCommKey
		for i := 0; i < len(e) && i < len(k.Comm)-1; i++ {
			k.Comm[i] = int8(e[i])
		}
		one := uint8(1)
		if err := b.objs.RootComms.Update(&k, &one, ebpf.UpdateAny); err != nil {
			return err
		}
	}
	return nil
}

// Watch makes pid a root, with the processes it already started: the kernel follows its
// forks from here on.
func (b *Backend) Watch(pid int) error {
	b.mu.Lock()
	b.roots[pid] = true
	b.mu.Unlock()
	root := uint32(pid)
	k := uint32(pid)
	if err := b.objs.Watched.Update(&k, &root, ebpf.UpdateAny); err != nil {
		return err
	}
	// Marked first, so a fork from here on is the kernel's to follow; then the children it
	// already has.
	for _, p := range descendants(pid) {
		k := uint32(p)
		if err := b.objs.Watched.Update(&k, &root, ebpf.UpdateAny); err != nil {
			return err
		}
	}
	return nil
}

// engineComms are the container engine's processes, by the comm the kernel gives them.
var engineComms = []string{"dockerd", "containerd", "buildkitd"}

// Lend counts the engine's processes toward root's session until Unlend. By comm, not
// pid: the engine may run in a pid namespace whose numbers /proc here does not show.
func (b *Backend) Lend(root int) error {
	r := uint32(root)
	for _, c := range engineComms {
		k := lentKey(c)
		if err := b.objs.LentComms.Update(&k, &r, ebpf.UpdateAny); err != nil {
			return err
		}
	}
	one, on := uint32(1), uint32(1)
	return b.objs.SensorConfig.Update(&one, &on, ebpf.UpdateAny)
}

// Unlend stops counting the engine toward root's session, if it is still lent to it.
func (b *Backend) Unlend(root int) error {
	for _, c := range engineComms {
		k := lentKey(c)
		var cur uint32
		if err := b.objs.LentComms.Lookup(&k, &cur); err == nil && int(cur) == root {
			_ = b.objs.LentComms.Delete(&k)
		}
	}
	var k sensorLentKey
	var v uint32
	if b.objs.LentComms.Iterate().Next(&k, &v) {
		return nil
	}
	one, off := uint32(1), uint32(0)
	return b.objs.SensorConfig.Update(&one, &off, ebpf.UpdateAny)
}

func lentKey(comm string) sensorLentKey {
	var k sensorLentKey
	for i := 0; i < len(comm) && i < len(k.Comm)-1; i++ {
		k.Comm[i] = int8(comm[i])
	}
	return k
}

// WatchCgroup counts the processes in cgroup id toward root's session.
func (b *Backend) WatchCgroup(id uint64, root int) error {
	r := uint32(root)
	return b.objs.WatchedCgroups.Update(&id, &r, ebpf.UpdateAny)
}

// Live reads the open flows' counters.
func (b *Backend) Live() []sensor.Flow {
	var (
		k   uint64
		v   sensorFlow
		out []sensor.Flow
	)
	it := b.objs.Flows.Iterate()
	for it.Next(&k, &v) {
		out = append(out, b.flow(k, &v))
	}
	return out
}

// LiveFiles reads the open files' counters.
func (b *Backend) LiveFiles() []sensor.FileOp {
	var (
		k   uint64
		v   sensorFileio
		out []sensor.FileOp
	)
	it := b.objs.Files.Iterate()
	for it.Next(&k, &v) {
		out = append(out, sensor.FileOp{
			Op: sensor.OpOpen, PID: int(v.Tgid), Root: int(v.Root), Path: cstr(v.Path[:]),
			Read: v.Rx, Written: v.Tx, Flags: v.Flags, Time: b.at(v.Start),
		})
	}
	return out
}

func (b *Backend) flow(id uint64, v *sensorFlow) sensor.Flow {
	f := sensor.Flow{
		ID: id, PID: int(v.Tgid), Root: int(v.Root), Comm: cstr(v.Comm[:]),
		TX: v.Tx, RX: v.Rx, Start: b.at(v.Start), Last: b.at(v.Last),
		Local:  netip.AddrPortFrom(addr(v.Family, v.Saddr), v.Sport),
		Remote: netip.AddrPortFrom(addr(v.Family, v.Daddr), v.Dport),
	}
	switch v.Proto {
	case unix.IPPROTO_TCP:
		f.Proto = "tcp"
	case unix.IPPROTO_UDP:
		f.Proto = "udp"
	default:
		f.Proto = fmt.Sprint(v.Proto)
	}
	return f
}

// Enforce replaces the deny maps with p and switches enforcement on or to audit.
func (b *Backend) Enforce(p sensor.Resolved) error {
	// What the policy denies is added before anything is removed, and only what no rule of
	// it names any more is removed: the addresses dns_ingress added for a host rule stay,
	// and no connect ever meets a map emptied for a rebuild.
	rules := map[uint32]bool{}
	hosts := map[uint64]bool{}
	for host, rule := range p.Hosts {
		h, r := DNSHash(host), uint32(rule)
		rules[r], hosts[h] = true, true
		if err := b.objs.DenyHosts.Update(&h, &r, ebpf.UpdateAny); err != nil {
			return err
		}
	}
	ips := map[sensorIpKey]bool{}
	for a, rule := range p.IPs {
		k := sensorIpKey{Family: unix.AF_INET6, Addr: a.As16()}
		if a.Is4() {
			k.Family, k.Addr = unix.AF_INET, [16]uint8{}
			v4 := a.As4()
			copy(k.Addr[:], v4[:])
		}
		r := uint32(rule)
		rules[r], ips[k] = true, true
		if err := b.objs.DenyIps.Update(&k, &r, ebpf.UpdateAny); err != nil {
			return err
		}
	}
	inodes := map[sensorInodeKey]bool{}
	for path, rule := range p.Paths {
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			continue // not there yet; the policy is re-resolved as it changes
		}
		k := sensorInodeKey{Ino: st.Ino, Dev: uint32(unix.Major(st.Dev)<<20 | unix.Minor(st.Dev))}
		r := uint32(rule)
		rules[r], inodes[k] = true, true
		if err := b.objs.DenyInodes.Update(&k, &r, ebpf.UpdateAny); err != nil {
			return err
		}
	}
	if err := prune(b.objs.DenyHosts, func(h uint64, _ uint32) bool { return hosts[h] }); err != nil {
		return err
	}
	if err := prune(b.objs.DenyIps, func(k sensorIpKey, r uint32) bool { return ips[k] || rules[r] }); err != nil {
		return err
	}
	if err := prune(b.objs.DenyInodes, func(k sensorInodeKey, _ uint32) bool { return inodes[k] }); err != nil {
		return err
	}
	zero, on := uint32(0), uint32(0)
	if p.Enforce {
		on = 1
	}
	return b.objs.SensorConfig.Update(&zero, &on, ebpf.UpdateAny)
}

// DNSHash is dns_ingress's hash of host in DNS wire form, lowercase: each byte from the
// last to the first, h = h*P + b, with 64-bit wraparound.
func DNSHash(host string) uint64 {
	var wire []byte
	for _, l := range strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".") {
		wire = append(append(wire, byte(len(l))), l...)
	}
	const p = 1099511628211
	var h uint64
	for i := len(wire) - 1; i >= 0; i-- {
		h = h*p + uint64(wire[i])
	}
	return h
}

// rwSyscalls numbers the read and write syscalls of the architecture terma runs on: 1 reads,
// 2 writes.
func rwSyscalls() map[uint32]uint32 {
	switch runtime.GOARCH {
	case "amd64": // read, write, pread64, pwrite64, readv, writev, preadv, pwritev
		return map[uint32]uint32{0: 1, 1: 2, 17: 1, 18: 2, 19: 1, 20: 2, 295: 1, 296: 2}
	default: // the generic table arm64 uses
		return map[uint32]uint32{63: 1, 64: 2, 65: 1, 66: 2, 67: 1, 68: 2, 69: 1, 70: 2}
	}
}

// prune deletes the entries of m that keep refuses.
func prune[K comparable](m *ebpf.Map, keep func(K, uint32) bool) error {
	var (
		k    K
		v    uint32
		gone []K
	)
	it := m.Iterate()
	for it.Next(&k, &v) {
		if !keep(k, v) {
			gone = append(gone, k)
		}
	}
	for i := range gone {
		if err := m.Delete(&gone[i]); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return err
		}
	}
	return it.Err()
}

func addr(family uint16, b [16]uint8) netip.Addr {
	if family == unix.AF_INET {
		return netip.AddrFrom4([4]byte(b[:4]))
	}
	return netip.AddrFrom16(b).Unmap()
}

func cstr(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

var _ sensor.Backend = (*Backend)(nil)
