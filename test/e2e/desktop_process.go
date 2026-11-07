package e2e

import (
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// The machine's processes, as ps lists them: a desktop app's process tree, and the chain from
// an agent up to the app that started it.

// Process is one running process.
type Process struct {
	PID, PPID int
	Command   string
}

// processes are the machine's processes, as ps lists them.
func processes() []Process {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	var ps []Process
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		rest := strings.TrimLeft(line, " \t")
		for range 2 { // past the two numbers, to the command line as ps printed it
			rest = strings.TrimLeft(rest[strings.IndexAny(rest, " \t"):], " \t")
		}
		ps = append(ps, Process{PID: pid, PPID: ppid, Command: strings.TrimRight(rest, "\n")})
	}
	return ps
}

// descendants are pid's processes' process tree, its children first.
func descendants(pid int) []Process {
	all := processes()
	var out []Process
	frontier := []int{pid}
	for len(frontier) > 0 {
		var next []int
		for _, p := range all {
			if slices.Contains(frontier, p.PPID) {
				out = append(out, p)
				next = append(next, p.PID)
			}
		}
		frontier = next
	}
	return out
}

// ancestry is the chain of processes from pid up to (not including) top, pid first, each its
// command line.
func ancestry(pid, top int) []string {
	byPID := map[int]Process{}
	for _, p := range processes() {
		byPID[p.PID] = p
	}
	var out []string
	for p, ok := byPID[pid]; ok && p.PID != top && len(out) < 16; p, ok = byPID[p.PPID] {
		out = append(out, p.Command)
	}
	return out
}
