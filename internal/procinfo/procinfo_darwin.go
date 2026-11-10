package procinfo

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Supported is true: sysctl and proc_info answer both questions.
const Supported = true

func parentOf(pid int) (int, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, false
	}
	return int(kp.Eproc.Ppid), true
}

// The kernel's proc_info call (sys/proc_info.h), its SDK layout pinned against a real
// socket by TestFindSenderNamesTheConnectingProcess.
const (
	sysProcInfo          = 336 // SYS_proc_info
	procInfoCallListPIDs = 1   // PROC_INFO_CALL_LISTPIDS
	procAllPIDs          = 1   // PROC_ALL_PIDS
	procInfoCallPIDInfo  = 2   // PROC_INFO_CALL_PIDINFO
	procInfoCallFDInfo   = 3   // PROC_INFO_CALL_PIDFDINFO
	procPIDListFDs       = 1   // PROC_PIDLISTFDS
	procPIDFDSocketInfo  = 3   // PROC_PIDFDSOCKETINFO
	proxFDTypeSocket     = 2   // PROX_FDTYPE_SOCKET
	procFDInfoSize       = 8   // sizeof(struct proc_fdinfo)
	socketFDInfoSize     = 792 // sizeof(struct socket_fdinfo)
	offSocketKind        = 256 // psi.soi_kind
	offForeignPort       = 264 // psi.soi_proto.pri_tcp.tcpsi_ini.insi_fport
	offLocalPort         = 268 // psi.soi_proto.pri_tcp.tcpsi_ini.insi_lport
	offVFlag             = 288 // psi.soi_proto.pri_tcp.tcpsi_ini.insi_vflag
	offForeignAddr       = 296 // psi.soi_proto.pri_tcp.tcpsi_ini.insi_faddr, 16 bytes
	offLocalAddr         = 312 // psi.soi_proto.pri_tcp.tcpsi_ini.insi_laddr, 16 bytes
	iniIPv4              = 0x1 // INI_IPV4: the addresses are in4in6_addr, IPv4 in the last 4 bytes
	sockInfoTCP          = 2   // SOCKINFO_TCP
	maxFDsPerProcess     = 4096
)

func procInfo(callnum, pid, flavor int, arg uint64, buf []byte) (int, bool) {
	if len(buf) == 0 {
		return 0, false
	}
	n, _, errno := syscall.Syscall6(sysProcInfo, uintptr(callnum), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 || int(n) <= 0 {
		return 0, false
	}
	return int(n), true
}

// ownsConn reports whether pid holds c's client end.
func ownsConn(pid int, c conn) bool {
	fds := make([]byte, procFDInfoSize*maxFDsPerProcess)
	n, ok := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, fds)
	if !ok {
		return false
	}
	info := make([]byte, socketFDInfoSize)
	for i := 0; i+procFDInfoSize <= n; i += procFDInfoSize {
		fd := int32(binary.LittleEndian.Uint32(fds[i:]))
		if binary.LittleEndian.Uint32(fds[i+4:]) != proxFDTypeSocket {
			continue
		}
		if m, ok := procInfo(procInfoCallFDInfo, pid, procPIDFDSocketInfo, uint64(fd), info); !ok || m < offLocalAddr+16 {
			continue
		}
		if binary.LittleEndian.Uint32(info[offSocketKind:]) != sockInfoTCP {
			continue
		}
		// insi_lport and insi_fport hold a port in network byte order in their low 16 bits.
		end := func(addrOff, portOff int) netip.AddrPort {
			raw := binary.LittleEndian.Uint32(info[portOff:])
			port := uint16(raw>>8) | uint16(raw<<8)
			a := [16]byte(info[addrOff : addrOff+16])
			if info[offVFlag]&iniIPv4 != 0 {
				return netip.AddrPortFrom(netip.AddrFrom4([4]byte(a[12:])), port)
			}
			return netip.AddrPortFrom(netip.AddrFrom16(a), port)
		}
		if c.is(end(offLocalAddr, offLocalPort), end(offForeignAddr, offForeignPort)) {
			return true
		}
	}
	return false
}

// allPIDs lists every process id the kernel reports.
func allPIDs() []int {
	buf := make([]byte, 4*16384)
	n, ok := procInfo(procInfoCallListPIDs, procAllPIDs, 0, 0, buf)
	if !ok {
		return nil
	}
	var out []int
	for i := 0; i+4 <= n; i += 4 {
		if pid := int(int32(binary.LittleEndian.Uint32(buf[i:]))); pid > 1 {
			out = append(out, pid)
		}
	}
	return out
}

// environ reads the environment pid was started with from KERN_PROCARGS2.
func environ(pid int) ([]string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Eproc.Ucred.Uid) != os.Getuid() {
		return nil, false
	}
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, false
	}
	return procArgsEnv(buf)
}

// procArgsEnv reads the environment from KERN_PROCARGS2's strings, those exec copied onto the
// process's stack: argc, the executable's path padded with NULs, argc arguments, then the
// environment.
func procArgsEnv(buf []byte) ([]string, bool) {
	if len(buf) < 4 {
		return nil, false
	}
	argc := int(binary.LittleEndian.Uint32(buf))
	rest := buf[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, false
	}
	rest = bytes.TrimLeft(rest[i:], "\x00")
	for range argc {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil, false
		}
		rest = rest[i+1:]
	}
	return splitEnv(rest), true
}
