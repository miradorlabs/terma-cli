package procinfo

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func parentOf(pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	// comm may hold spaces and parentheses, so read after the last ')'.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}

// Supported is true: /proc answers both questions.
const Supported = true

// ownsConn reports whether pid holds c's client end.
func ownsConn(pid int, c conn) bool {
	inodes := map[string]bool{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(table)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) <= 9 {
				continue
			}
			local, lok := procAddr(fields[1])
			remote, rok := procAddr(fields[2])
			if lok && rok && c.is(local, remote) {
				inodes["socket:["+fields[9]+"]"] = true
			}
		}
		_ = f.Close()
	}
	if len(inodes) == 0 {
		return false
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if link, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil && inodes[link] {
			return true
		}
	}
	return false
}

// allPIDs lists the processes in /proc.
func allPIDs() []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 1 {
			out = append(out, pid)
		}
	}
	return out
}

// procAddr reads /proc/net/tcp's ADDR:PORT: the port in hex, the address as hex 32-bit words
// each printed in the host's byte order (four for tcp6, one for tcp).
func procAddr(s string) (netip.AddrPort, bool) {
	hexAddr, hexPort, ok := strings.Cut(s, ":")
	raw, err := hex.DecodeString(hexAddr)
	port, perr := strconv.ParseUint(hexPort, 16, 16)
	if !ok || err != nil || perr != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(raw[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	addr, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(addr, uint16(port)), true
}
