package procinfo

import (
	"bufio"
	"fmt"
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
	// pid (comm) state ppid ... — comm may hold spaces and parentheses, so read after
	// the last ')'.
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

// ownsPort reports whether pid holds a TCP socket bound locally to port: its inode in
// /proc/net/tcp{,6}, then among pid's own descriptors.
func ownsPort(pid, port int) bool {
	inodes := map[string]bool{}
	suffix := fmt.Sprintf(":%04X", port)
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(table)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) > 9 && strings.HasSuffix(fields[1], suffix) {
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
