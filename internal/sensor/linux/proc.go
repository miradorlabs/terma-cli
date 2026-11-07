//go:build linux

package linux

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/miradorlabs/terma-cli/internal/sensor"
)

// descendants lists the processes pid already started, read from /proc: the kernel only
// follows forks from the moment a root is watched.
func descendants(pid int) []int {
	children := map[int][]int{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pp := ppid(p); pp > 0 {
			children[pp] = append(children[pp], p)
		}
	}
	var out []int
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

func ppid(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// pid (comm) state ppid ...; comm may hold spaces and parentheses.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// CgroupOf is the cgroup v2 id of pid's cgroup: the inode of its directory under
// /sys/fs/cgroup, which is what bpf_get_current_cgroup_id returns.
func CgroupOf(pid int) (uint64, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if path, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			var st unix.Stat_t
			if err := unix.Stat(filepath.Join("/sys/fs/cgroup", path), &st); err != nil {
				return 0, err
			}
			return st.Ino, nil
		}
	}
	return 0, os.ErrNotExist
}

// Running lists the processes already running whose executable's base name is in exes.
func Running(exes []string) []sensor.Proc {
	want := map[string]bool{}
	for _, e := range exes {
		want[e] = true
	}
	var out []sensor.Proc
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink("/proc/" + e.Name() + "/exe")
		if err != nil || !want[filepath.Base(exe)] {
			continue
		}
		// A root's own children running the same executable belong to its session.
		if pexe, _ := os.Readlink("/proc/" + strconv.Itoa(ppid(pid)) + "/exe"); want[filepath.Base(pexe)] {
			continue
		}
		args, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		out = append(out, sensor.Proc{PID: pid, PPID: ppid(pid), Exe: exe, Args: strings.Split(strings.TrimRight(string(args), "\x00"), "\x00"), Start: time.Now()})
	}
	return out
}
