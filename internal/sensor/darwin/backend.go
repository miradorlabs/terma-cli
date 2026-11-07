//go:build darwin

// Package darwin is the sensor's macOS backend: TermaSensor.app's two system extensions (a
// content filter and an Endpoint Security client) report to it over loopback, and it hands
// them the roots and the policy. The extensions keep the process trees, so a root starts
// its session in the extension, before it can fork, as the eBPF programs do on Linux.
package darwin

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/sensor"
)

// Addr is where the extensions find the sensor.
const Addr = "127.0.0.1:7781"

// Backend serves the extensions.
type Backend struct {
	log *slog.Logger

	mu        sync.Mutex
	conns     map[string]net.Conn // by extension: "network", "security"
	rootNames []string
	roots     map[int]bool
	policy    map[string]any // per extension, the policy part it enforces
	live      map[string]sensor.Flow
	sink      sensor.Sink
}

// New is a backend with no extension connected yet.
func New(log *slog.Logger) *Backend {
	return &Backend{
		log: log, conns: map[string]net.Conn{}, roots: map[int]bool{},
		policy: map[string]any{}, live: map[string]sensor.Flow{},
	}
}

// Run accepts the extensions until ctx ends.
func (b *Backend) Run(ctx context.Context, sink sensor.Sink) error {
	b.mu.Lock()
	b.sink = sink
	b.mu.Unlock()
	ln, err := net.Listen("tcp", Addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go b.serve(c)
	}
}

func (b *Backend) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var ext string
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev["k"] == "hello" {
			ext, _ = ev["ext"].(string)
			b.log.Info("extension connected", "ext", ext, "pid", num(ev["pid"]))
			b.mu.Lock()
			b.conns[ext] = c
			b.mu.Unlock()
			b.push(ext)
			continue
		}
		b.event(ev)
	}
	b.mu.Lock()
	if b.conns[ext] == c {
		delete(b.conns, ext)
	}
	b.mu.Unlock()
	b.log.Warn("extension disconnected", "ext", ext)
}

// push sends ext everything it needs: the root names and roots, and its part of the policy.
func (b *Backend) push(ext string) {
	b.mu.Lock()
	c := b.conns[ext]
	roots := make([]int, 0, len(b.roots))
	for r := range b.roots {
		roots = append(roots, r)
	}
	sort.Ints(roots)
	cfg := map[string]any{"root_names": b.rootNames, "roots": roots}
	if p, ok := b.policy[ext]; ok {
		cfg["policy"] = p
	}
	b.mu.Unlock()
	if c == nil {
		return
	}
	line, _ := json.Marshal(cfg)
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(append(line, '\n')); err != nil {
		b.log.Warn("push", "ext", ext, "err", err)
	}
}

func (b *Backend) pushAll() {
	b.push("network")
	b.push("security")
}

// Roots has the Endpoint Security extension start a session at every exec of one of exes.
func (b *Backend) Roots(exes []string) error {
	b.mu.Lock()
	b.rootNames = exes
	b.mu.Unlock()
	b.pushAll()
	return nil
}

// Watch adopts a running process as a root; the processes it already started are not
// followed, only those it starts from now.
func (b *Backend) Watch(pid int) error {
	b.mu.Lock()
	b.roots[pid] = true
	c := b.conns["security"]
	b.mu.Unlock()
	if c != nil {
		line, _ := json.Marshal(map[string]any{"adopt": []int{pid}})
		_, _ = c.Write(append(line, '\n'))
	}
	b.push("network")
	return nil
}

// WatchCgroup has no macOS meaning: a container runs in the engine's Linux VM, and its
// traffic leaves through the engine's own process.
func (b *Backend) WatchCgroup(uint64, int) error {
	return errors.New("containers run in the engine's VM on macOS; attribute them through the engine's process")
}

// Live is the open flows, at the counts the filter last reported.
func (b *Backend) Live() []sensor.Flow {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]sensor.Flow, 0, len(b.live))
	for _, f := range b.live {
		out = append(out, f)
	}
	return out
}

// Lend has no macOS counterpart yet: the engine's VM reaches the network through the
// engine's own helper process, which the filter would have to lend instead.
func (b *Backend) Lend(int) error { return nil }

// Unlend does nothing, as Lend.
func (b *Backend) Unlend(int) error { return nil }

// LiveFiles is nil: Endpoint Security says a file changed, never by how many bytes.
func (b *Backend) LiveFiles() []sensor.FileOp { return nil }

// Enforce splits the policy between the extensions: hosts and addresses to the filter,
// paths to the Endpoint Security client.
func (b *Backend) Enforce(p sensor.Resolved) error {
	hosts := make([]string, 0, len(p.Hosts))
	for h := range p.Hosts {
		hosts = append(hosts, h)
	}
	ips := make([]string, 0, len(p.IPs))
	for a := range p.IPs {
		ips = append(ips, a.String())
	}
	paths := make([]string, 0, len(p.Paths))
	for path := range p.Paths {
		paths = append(paths, path)
	}
	b.mu.Lock()
	b.policy["network"] = map[string]any{"enforce": p.Enforce, "hosts": hosts, "ips": ips}
	b.policy["security"] = map[string]any{"enforce": p.Enforce, "paths": paths}
	b.mu.Unlock()
	b.pushAll()
	return nil
}

func (b *Backend) event(ev map[string]any) {
	b.mu.Lock()
	sink := b.sink
	b.mu.Unlock()
	if sink == nil {
		return
	}
	at := time.UnixMilli(int64(num(ev["t"])))
	pid, root := num(ev["pid"]), num(ev["root"])
	switch ev["k"] {
	case "exec":
		if pid == root {
			b.mu.Lock()
			isNew := !b.roots[root]
			b.roots[root] = true
			b.mu.Unlock()
			if isNew {
				b.push("network")
			}
		}
		sink.Exec(sensor.Proc{PID: pid, PPID: num(ev["ppid"]), Root: root, Exe: str(ev["path"]), Args: strs(ev["args"]), Start: at})
	case "fork":
		sink.Fork(sensor.Proc{PID: pid, PPID: num(ev["ppid"]), Root: root, Start: at})
	case "exit":
		sink.Exit(pid, root, at)
	case "flow_open", "flow_stats", "flow_close", "flow_hello", "flow_block":
		b.flow(ev, sink, at)
	case "close":
		op := sensor.FileOp{Op: sensor.OpClose, PID: pid, Root: root, Comm: str(ev["comm"]), Path: str(ev["path"]), Time: at}
		if ev["modified"] == true {
			// Endpoint Security says a file changed, not by how much: the size it closed at
			// bounds what was written.
			op.Written = uint64(num(ev["size"]))
		}
		sink.File(op)
	case "create", "unlink", "rename":
		op := sensor.FileOp{Op: str(ev["k"]), PID: pid, Root: root, Comm: str(ev["comm"]), Path: str(ev["path"]), To: str(ev["to"]), Time: at}
		if op.Op == "create" {
			return // a create is followed by the file's close
		}
		sink.File(op)
	case "beat":
		b.log.Info("beat", "ev", ev)
	case "open_block":
		sink.Blocked(sensor.Block{Kind: "open", PID: pid, Root: root, Target: str(ev["path"]), Enforced: ev["enforced"] == true, Time: at})
	}
}

func (b *Backend) flow(ev map[string]any, sink sensor.Sink, at time.Time) {
	id := str(ev["id"])
	b.mu.Lock()
	f, known := b.live[id]
	if !known {
		f = sensor.Flow{
			ID: uint64(len(b.live) + 1), PID: num(ev["pid"]), Root: num(ev["root"]), Comm: base(str(ev["path"])),
			Proto: str(ev["proto"]), Remote: addrPort(str(ev["remote"])), Local: addrPort(str(ev["local"])),
			Host: str(ev["host"]), Start: at,
		}
	}
	if tx, rx := uint64(num(ev["tx"])), uint64(num(ev["rx"])); tx+rx > 0 {
		f.TX, f.RX = tx, rx
	}
	f.Last = at
	k := str(ev["k"])
	if k == "flow_close" {
		delete(b.live, id)
	} else if k != "flow_block" || ev["enforced"] != true {
		b.live[id] = f
	}
	b.mu.Unlock()

	switch k {
	case "flow_open":
		if f.Host != "" {
			sink.Hello(f.Local, f.Remote, f.Host)
		}
	case "flow_hello":
		raw, _ := base64.StdEncoding.DecodeString(str(ev["hello"]))
		if host := sensor.ClientHelloSNI(raw); host != "" {
			sink.Hello(f.Local, f.Remote, host)
		}
	case "flow_close":
		f.Closed = true
		sink.FlowClosed(f)
	case "flow_block":
		target := f.Host
		if target == "" {
			target = f.Remote.String()
		}
		sink.Blocked(sensor.Block{Kind: "connect", PID: f.PID, Root: f.Root, Comm: f.Comm, Target: target, Enforced: ev["enforced"] == true, Time: at})
	}
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	var out []string
	for _, x := range asSlice(v) {
		out = append(out, str(x))
	}
	return out
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func base(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

func addrPort(s string) netip.AddrPort {
	ap, _ := netip.ParseAddrPort(s)
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

var _ sensor.Backend = (*Backend)(nil)
