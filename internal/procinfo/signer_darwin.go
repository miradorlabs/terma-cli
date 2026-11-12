package procinfo

import (
	"encoding/binary"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sysCsops is SYS_csops: a process's code signing status and identity, as the kernel
// checked its pages when they loaded (what `codesign --pid` reads). No file is read.
const sysCsops = 169

const (
	csopsStatus   = 0  // CS_OPS_STATUS: the flags
	csopsIdentity = 11 // CS_OPS_IDENTITY: the signing identifier
	csopsTeamID   = 14 // CS_OPS_TEAMID: the developer's team id
	csValid       = 0x1
	csPlatform    = 0x0400_0000
)

// Signer is who signed pid while it runs, and when it started (so a pid used again is
// another process); false when it is gone or the kernel will not say.
func Signer(pid int) (Signature, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return Signature{}, false
	}
	var flags uint32
	if _, _, errno := syscall.Syscall6(sysCsops, uintptr(pid), csopsStatus, uintptr(unsafe.Pointer(&flags)), 4, 0, 0); errno != 0 {
		return Signature{}, false
	}
	return Signature{
		Start:      kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec),
		Valid:      flags&csValid != 0,
		Platform:   flags&csPlatform != 0,
		Identifier: csopsBlob(pid, csopsIdentity),
		TeamID:     csopsBlob(pid, csopsTeamID),
	}, true
}

// csopsBlob is the text of a csops blob: an 8-byte header (magic, then the blob's length
// big-endian), then the text.
func csopsBlob(pid int, op uintptr) string {
	buf := make([]byte, 1032)
	if _, _, errno := syscall.Syscall6(sysCsops, uintptr(pid), op, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0); errno != 0 {
		return ""
	}
	n := int(binary.BigEndian.Uint32(buf[4:8]))
	if n <= 8 || n > len(buf) {
		return ""
	}
	text := buf[8:n]
	for i, b := range text {
		if b == 0 {
			text = text[:i]
			break
		}
	}
	return string(text)
}
