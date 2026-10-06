package procinfo

import (
	"encoding/binary"
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
	offLocalPort         = 268 // psi.soi_proto.pri_tcp.tcpsi_ini.insi_lport
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

// ownsPort reports whether pid holds a TCP socket bound locally to port.
func ownsPort(pid, port int) bool {
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
		if m, ok := procInfo(procInfoCallFDInfo, pid, procPIDFDSocketInfo, uint64(fd), info); !ok || m < offLocalPort+4 {
			continue
		}
		if binary.LittleEndian.Uint32(info[offSocketKind:]) != sockInfoTCP {
			continue
		}
		// insi_lport holds the port in network byte order in its low 16 bits.
		raw := binary.LittleEndian.Uint32(info[offLocalPort:])
		if int(uint16(raw>>8)|uint16(raw<<8)) == port {
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
