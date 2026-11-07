package e2e

import (
	"cmp"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The files profile: what a harness build does on the file system, and which programs and
// sockets it uses, recorded by tracing its process tree while it runs a turn. Linux traces
// with strace (filetrace_strace.go), macOS with Endpoint Security's eslogger and lsof
// (filetrace_eslogger.go); each tracer's output becomes the same TraceEvents, and FilesOf
// makes the published shape of them: every path the build read, wrote, created, deleted,
// renamed or looked for, with its run's directories in placeholders; every program it started,
// with the commands that started it, whether it was the build's own doing or the tool call's,
// and what it is for where the command is a well-known one (execPurposes); whether it touched
// the decoy credentials the scenario planted (decoys.go); the sockets it listened on and
// connected to. What is not the build's is dropped and counted (dropReason): the kernel's
// files, shared libraries and the system's own files every process reads, the build's
// installation, the hook recorder the scenario registers (its shell, its process and its
// files: the snapshot's hooks section says what the build hands a hook), the test's own files,
// the PATH searched (Lookups says for what), what a program the build starts does outside the
// run's own places, bubblewrap building a sandbox's root. reference/SCHEMA.md says the same for
// readers.

// TraceEvent is one thing a traced process did.
type TraceEvent struct {
	Pid int
	At  time.Time
	// Kind is what it did: open, stat, list, lookup (macOS: a path looked up, whether or not
	// it exists), read and write (a file's content, through a descriptor), mkdir, unlink,
	// rmdir, rename, link, chdir, exec, fork, connect, bind, listen, dns, xpc, launch-item,
	// tcc, task-port.
	Kind string
	Path string // absolute, where the tracer could resolve it
	To   string // a rename's or link's destination
	// An open's mode.
	Read, Write, Create bool
	// Err is a failed call's errno name ("ENOENT"); a tracer that sees no failures leaves it.
	Err string
	// Argv is an exec's arguments; Child and Thread a fork's new process and whether it is a
	// thread of the same process; Ppid the parent a tracer names on every event (eslogger).
	Argv   []string
	Child  int
	Thread bool
	Ppid   int
	// Sock is a socket call's: its protocol, and the address it named.
	Sock *TraceSocket
	// Name is what a macOS event names: an XPC service, a TCC service, a launch item's
	// executable, a task port's process.
	Name string
	// Count is how many events the tracer lost (Kind "lost"): Endpoint Security drops what a
	// client too slow for the machine's events has not read.
	Count int
}

// TraceSocket is the socket a call used and the address it named.
type TraceSocket struct {
	Proto string // tcp | udp | unix
	Host  string // an IP address, or a Unix socket's path ("@name" abstract)
	Port  int
	// Ephemeral marks a bind that asked for any port.
	Ephemeral bool
}

// TraceScope is what FilesOf needs to know of the run to tell the build's own activity from
// the rest, and to name what it found.
type TraceScope struct {
	Platform, Tracer string
	// Root is the harness's process; the traced tree is its descendants.
	Root int
	// Replace maps a literal of the run (a sandbox path) to its placeholder, as the profile's
	// placeholders do.
	Replace map[string]string
	// Self are the build's own installation: its binary and the folder it sits in.
	Self []string
	// Hooks are the hook recorder the scenario registered for every hook event: its executable
	// and its folder. A process that runs it, or a shell whose command line names it (the
	// harness running the hook's command), and everything it starts are dropped ("hooks"), and
	// so is every path under them.
	Hooks []string
	// Tools are the commands the scenario's fixture model told the build's tool calls to run: a
	// process whose command line holds one, everything it starts and the commands on the way to
	// it from the build (a sandbox and its helpers) are the tool's (role "tool"); the rest is the
	// build's own ("harness").
	Tools []string
	// Test are folders and files that are the test's own (the hook payloads it records, the
	// checkout the suite runs from, with the builds it downloads and the logs it writes).
	Test []string
	// TestWay are the folders on the way to the test's own, below the home it runs in: a build
	// resolving its own path looks each one up.
	TestWay []string
	// PathDirs are the PATH the build ran with: what it looks for there is a program, not a
	// path.
	PathDirs []string
	// Path is that PATH as one value: in a command line (a shell's snapshot exports it) it is
	// <path>, never the machine's list of what it has installed.
	Path string
	// Turn is the run's model calls: from the first request to the last response.
	TurnStart, TurnEnd time.Time
	// Decoys are the planted files and sockets, by absolute path.
	Decoys []string
	// Endpoints name the run's own loopback addresses ("127.0.0.1:43210" → "<provider>").
	Endpoints map[string]string
}

// FilesProbe is the files profile of one build on one platform, the probe "filesystem".
type FilesProbe struct {
	Platform string `json:"platform"`
	Tracer   string `json:"tracer"`
	// Paths are every path the build touched, normalized, with what it did to each.
	Paths map[string]*PathUse `json:"paths"`
	// Exec are the programs the build started, by their normalized command line.
	Exec map[string]*ExecUse `json:"exec"`
	// Lookups are the programs the build looked for on its PATH, found or not.
	Lookups []string `json:"lookups"`
	// Decoys are the planted credentials, each with the most the build did to it.
	Decoys  map[string]string `json:"decoys"`
	Sockets *SocketUse        `json:"sockets"`
	// MacOS is what only Endpoint Security sees: XPC services, launch items, TCC changes,
	// task ports.
	MacOS *MacOSUse `json:"macos,omitempty"`
	// Dropped counts the events left out, by why.
	Dropped map[string]int `json:"dropped"`
}

// PathUse is what the build did to one path, and when.
type PathUse struct {
	Ops    []string `json:"ops"`
	Phases []string `json:"phases"`
	// Programs are the programs other than the build itself that touched it (git, bash).
	Programs []string `json:"programs,omitempty"`
}

// ExecUse is one program the build started.
type ExecUse struct {
	Argv   []string `json:"argv"`
	Phases []string `json:"phases"`
	Count  int      `json:"count"`
	// By is the program that started it: the build, or a shell it started.
	By string `json:"by"`
	// Parents are the exec keys of the commands that started it, sorted; empty where the build
	// itself did. ExecBuild, first, stands for the build among them where a command started it
	// too.
	Parents []string `json:"parents"`
	// Role is whose doing it was: RoleHarness, RoleTool, or RoleBoth where it ran as each.
	Role string `json:"role"`
	// Purpose is what it is for, where its command is a well-known one (execPurposes) or a
	// command that started it is one whose programs share its purpose.
	Purpose string `json:"purpose,omitempty"`
}

// The roles of a program the build started.
const (
	// RoleHarness is the build acting on its own: its startup, its context, its shell snapshot.
	RoleHarness = "harness"
	// RoleTool is the command the scenario's fixture model told a tool call to run, and
	// everything around it: the shell, sandbox and helpers between the build and it, the
	// programs it starts.
	RoleTool = "tool"
	// RoleBoth is a command line that ran as each.
	RoleBoth = "both"
)

// ExecBuild stands for the build itself among a command's parents, where a command started it
// as well: a command the build alone started has no parents.
const ExecBuild = "<build>"

// SocketUse is what the build did with sockets.
type SocketUse struct {
	// Listening are the addresses it listened on, "tcp 127.0.0.1:<ephemeral>" or a fixed port.
	Listening map[string]*SockUse `json:"listening"`
	// Connections are where it connected: the run's own endpoints by name (<provider>,
	// <proxy>), the resolver, anything else as <local> or <remote> and its port.
	Connections map[string]*SockUse `json:"connections"`
	// Unix are the Unix sockets it connected to or bound, by path, with their role.
	Unix map[string]*SockUse `json:"unix"`
	// DNS are the names it looked up, where the query could be read.
	DNS []string `json:"dns"`
}

// SockUse is one socket address, and when.
type SockUse struct {
	Role     string   `json:"role,omitempty"`
	Ops      []string `json:"ops,omitempty"` // a Unix socket's: connect, bind
	Phases   []string `json:"phases"`
	Programs []string `json:"programs,omitempty"`
	Count    int      `json:"count"`
	// Missing marks a socket every attempt found absent (a resolver cache that is not there).
	Missing bool `json:"missing,omitempty"`
}

// MacOSUse is what Endpoint Security alone sees, by name.
type MacOSUse struct {
	XPC         map[string]*SockUse `json:"xpc"`
	LaunchItems map[string]*SockUse `json:"launch_items"`
	TCC         map[string]*SockUse `json:"tcc"`
	TaskPorts   map[string]*SockUse `json:"task_ports"`
}

// The ops a path can have, in the order they are listed.
var pathOps = []string{"probe", "stat", "list", "read", "write", "create", "delete", "rename", "chdir", "denied"}

// decoyLevels rank what a build did to a decoy: the most it did is recorded.
var decoyLevels = []string{"not touched", "stat", "opened", "read", "connected", "written", "deleted"}

// The phases of a run, against its model calls.
const (
	PhaseStartup = "startup" // before the first model request
	PhaseTurn    = "turn"    // from the first request to the last response
	PhaseExit    = "exit"    // after the last response
)

func (s TraceScope) phase(at time.Time) string {
	switch {
	case s.TurnStart.IsZero() || at.Before(s.TurnStart):
		return PhaseStartup
	case s.TurnEnd.IsZero() || !at.After(s.TurnEnd):
		return PhaseTurn
	}
	return PhaseExit
}

// traceProc is a traced process as FilesOf follows it.
type traceProc struct {
	program string // the basename of what it runs
	exe     string // the path it executed
	own     bool   // it runs the build's own binary
	hook    bool   // the hook recorder's, or started by it
	tool    bool   // the tool call's command, or started by it
	execs   int
	argv    []string // the command line it was started with
	// run is the recorded run of the command it runs: nil for the build's own process, which
	// the exec list does not hold.
	run *execRun
}

// execRun is one run of a recorded command: its exec key and normalized command line, the run
// that started it (nil: the build), and whether it is the tool call's.
type execRun struct {
	key    string
	argv   []string
	parent *execRun
	tool   bool
	// context is the purpose a program it starts takes where it has none of its own.
	context string
}

// parentKey is the exec key of the command that started r, "" for the build.
func (r *execRun) parentKey() string {
	if r.parent == nil {
		return ""
	}
	return r.parent.key
}

// FilesOf is the files profile of events, as scope says to read them.
func FilesOf(events []TraceEvent, scope TraceScope) *FilesProbe {
	out := &FilesProbe{Platform: scope.Platform, Tracer: scope.Tracer, Paths: map[string]*PathUse{}, Exec: map[string]*ExecUse{},
		Lookups: []string{}, Decoys: map[string]string{}, Dropped: map[string]int{},
		Sockets: &SocketUse{Listening: map[string]*SockUse{}, Connections: map[string]*SockUse{}, Unix: map[string]*SockUse{}, DNS: []string{}}}
	for _, d := range scope.Decoys {
		out.Decoys[scope.normalize(d)] = decoyLevels[0]
	}
	tree := traceTree(events, scope.Root)
	// strace follows the tree itself: every process it saw is the build's.
	tree.all = scope.Tracer == "strace"
	procs := map[int]*traceProc{}
	// A process, its thread's or its parent's state where it is new: a child starts as its
	// parent was when it forked.
	var procOf func(pid int) *traceProc
	procOf = func(pid int) *traceProc {
		if p := procs[pid]; p != nil {
			return p
		}
		p := &traceProc{program: scope.harness(), own: true}
		if parent, ok := tree.parent[pid]; ok && parent != pid {
			pp := procOf(parent)
			if tree.thread[pid] {
				procs[pid] = pp
				return pp
			}
			cp := *pp
			cp.execs = 0
			p = &cp
		}
		procs[pid] = p
		return p
	}
	seenAbsent := map[string]bool{}
	lookups := map[string]bool{}
	dns := map[string]bool{}
	// Every run of a recorded command, in the order they started: a run's parent comes first.
	var runs []*execRun
	for _, ev := range events {
		if ev.Kind == "lost" {
			out.Dropped["lost"] += ev.Count // not the build's to drop: the trace is short of them
			continue
		}
		if !tree.in(ev.Pid) {
			continue
		}
		p := procOf(ev.Pid)
		phase := scope.phase(ev.At)
		if ev.Kind == "exec" {
			if ev.Err != "" {
				// A program looked for along the PATH, there or not: the lookups say which.
				out.Dropped["path-search"]++
				continue
			}
			if p.hook {
				out.Dropped["hooks"]++
				continue
			}
			// The root's first exec is the scenario's own command.
			if ev.Pid == scope.Root && p.execs == 0 && p.own {
				p.execs++
				p.exe = ev.Path
				continue
			}
			by := p.program
			exe := ev.Path
			if exe == "/proc/self/exe" {
				exe = p.exe // a build starting itself again
			}
			// A stub handing its command line to the real program of its name (macOS's
			// /usr/bin/git running Xcode's git): the one program, started once.
			if !p.own && p.argv != nil && programName(ev, exe) == p.program && len(ev.Argv) > 0 && slices.Equal(ev.Argv[1:], p.argv[1:]) {
				p.exe = exe
				continue
			}
			np := &traceProc{program: programName(ev, exe), exe: exe, execs: p.execs + 1, argv: ev.Argv}
			switch {
			case scope.isHook(ev.Path, ev.Argv):
				// The hook recorder, or the shell the build runs its command line in: neither is
				// recorded, nor anything it does or starts.
				np.hook = true
				out.Dropped["hooks"]++
			default:
				np.own = scope.isSelf(exe)
				argv := scope.normalizeArgv(ev.Argv, ev.Path, np.own)
				key := strings.Join(argv, " ")
				np.tool = p.tool || scope.isTool(ev.Argv)
				np.run = &execRun{key: key, argv: argv, parent: p.run, tool: np.tool}
				runs = append(runs, np.run)
				e := out.Exec[key]
				if e == nil {
					e = &ExecUse{Argv: argv, By: by, Parents: []string{}}
					out.Exec[key] = e
				}
				e.Count++
				e.Phases = addWord(e.Phases, phase)
				if np.own {
					np.program = scope.harness()
				}
			}
			if tree.thread[ev.Pid] {
				*p = *np
			} else {
				procs[ev.Pid] = np
			}
			continue
		}
		if p.hook {
			out.Dropped["hooks"]++
			continue
		}
		// bubblewrap building a sandbox's root, before it runs the command in it: its mounts
		// are its command line's, which the exec records.
		if p.program == "bwrap" && ev.Kind != "fork" {
			out.Dropped["sandbox-setup"]++
			continue
		}
		program := ""
		if !p.own {
			program = p.program
		}
		switch ev.Kind {
		case "fork":
			continue
		case "connect", "bind", "listen", "dns":
			scope.socketEvent(out, ev, phase, program, dns)
			continue
		case "xpc", "launch-item", "tcc", "task-port":
			// A task port of the build's own process tree is its own business.
			if ev.Kind == "task-port" && tree.in(ev.Child) {
				continue
			}
			scope.macEvent(out, ev, phase, program)
			continue
		}
		if ev.Path == "" {
			continue
		}
		op := traceOp(ev, seenAbsent)
		if op == "" {
			continue
		}
		scope.decoyEvent(out, ev, op)
		paths := []struct{ path, op string }{{ev.Path, op}}
		if ev.Kind == "rename" && ev.Err == "" && ev.To != "" {
			paths = append(paths, struct{ path, op string }{ev.To, "write"})
			scope.decoyEvent(out, TraceEvent{Kind: "write", Path: ev.To}, "write")
		}
		for _, x := range paths {
			if why := scope.dropReason(x.path); why != "" {
				out.Dropped[why]++
				continue
			}
			// A program looked for along the PATH, by the build itself: a program, not a path.
			if dir, name := filepath.Split(x.path); slices.Contains(scope.PathDirs, filepath.Clean(dir)) && (x.op == "probe" || x.op == "stat") {
				if p.own {
					lookups[name] = true
				}
				out.Dropped["path-search"]++
				continue
			}
			key := scope.normalize(x.path)
			// A program the build starts is recorded for what it touches of the run's own places
			// (the home, the repository, the harness's folders, the temporary folder): its own
			// installation and the system's files are its own.
			if program != "" && !strings.HasPrefix(key, "<") {
				out.Dropped["helpers"]++
				continue
			}
			u := out.Paths[key]
			if u == nil {
				u = &PathUse{}
				out.Paths[key] = u
			}
			u.Ops = addWord(u.Ops, x.op)
			u.Phases = addWord(u.Phases, phase)
			if program != "" {
				u.Programs = addWord(u.Programs, program)
			}
		}
	}
	// A path the build looked for and then made, opened or checked is not "probe": what it did
	// says it. (On macOS a path only looked up is "probe": whether it was there is not known.)
	for _, u := range out.Paths {
		if len(u.Ops) > 1 {
			u.Ops = slices.DeleteFunc(u.Ops, func(op string) bool { return op == "probe" })
		}
	}
	// A folder the build made in the temporary folder with mkdtemp's six random characters
	// after a dash is the run's, whatever its letters (fswatch-probe-hjcdgr).
	for _, key := range slices.Sorted(maps.Keys(out.Paths)) {
		u := out.Paths[key]
		if u == nil || !strings.HasPrefix(key, "<tmp>/") || !slices.Contains(u.Ops, "create") || !mkdtempRE.MatchString(key) {
			continue
		}
		renamed := key[:len(key)-6] + "<id>"
		for _, k := range slices.Sorted(maps.Keys(out.Paths)) {
			if k != key && !strings.HasPrefix(k, key+"/") {
				continue
			}
			to := renamed + strings.TrimPrefix(k, key)
			v := out.Paths[k]
			delete(out.Paths, k)
			if w := out.Paths[to]; w != nil {
				for _, op := range v.Ops {
					w.Ops = addWord(w.Ops, op)
				}
				for _, ph := range v.Phases {
					w.Phases = addWord(w.Phases, ph)
				}
				for _, pr := range v.Programs {
					w.Programs = addWord(w.Programs, pr)
				}
				continue
			}
			out.Paths[to] = v
		}
	}
	// SQLite removes a database's -wal, -shm and -journal files when its last connection closes:
	// whether that came before the build exited is timing, not what the build does.
	for key, u := range out.Paths {
		if strings.HasSuffix(key, "-wal") || strings.HasSuffix(key, "-shm") || strings.HasSuffix(key, "-journal") {
			if len(u.Ops) > 1 {
				u.Ops = slices.DeleteFunc(u.Ops, func(op string) bool { return op == "delete" })
			}
		}
	}
	// The temporary folder is the machine's, shared with every other program and every run
	// before this one. Whether a path the build makes when it is missing was there already is
	// the machine's state, so its create reads as a stat either way. What the build only found
	// there and looked at (another session's folders, which it lists as it cleans up) is the
	// machine's, not the build's; what it made, read, wrote or removed, what it looked for and
	// did not find, and the folders on the way to what it made are its own.
	var made []string
	for key, u := range out.Paths {
		if strings.HasPrefix(key, "<tmp>/") && (slices.Contains(u.Ops, "create") || slices.Contains(u.Ops, "write")) {
			made = append(made, key)
		}
	}
	for key, u := range out.Paths {
		if !strings.HasPrefix(key, "<tmp>/") {
			continue
		}
		if i := slices.Index(u.Ops, "create"); i >= 0 {
			u.Ops = addWord(slices.Delete(u.Ops, i, i+1), "stat")
		}
		if slices.Contains(made, key) || slices.ContainsFunc(made, func(m string) bool { return strings.HasPrefix(m, key+"/") }) {
			continue
		}
		if !slices.ContainsFunc(u.Ops, func(op string) bool { return op != "stat" && op != "list" }) {
			delete(out.Paths, key)
			out.Dropped["machine"]++
		}
	}
	out.Lookups = append([]string{}, slices.Sorted(maps.Keys(lookups))...)
	out.Sockets.DNS = append([]string{}, slices.Sorted(maps.Keys(dns))...)
	for _, u := range out.Paths {
		sortWords(u.Ops, pathOps)
		sortWords(u.Phases, []string{PhaseStartup, PhaseTurn, PhaseExit})
		slices.Sort(u.Programs)
	}
	// The commands on the way from the build to a tool call's command are the tool's too: the
	// sandbox and helpers that run it, whose own command lines name it where the tracer read
	// them whole (eslogger) and not where it cut a long argument short (strace).
	for _, r := range runs {
		if r.tool {
			for up := r.parent; up != nil && !up.tool; up = up.parent {
				up.tool = true
			}
		}
	}
	// And so is everything those start (a second bubblewrap the helper runs).
	for _, r := range runs {
		if r.parent != nil && r.parent.tool {
			r.tool = true
		}
	}
	// Each command's parents ("" the build), roles and purpose, from its runs in order: a
	// well-known command's purpose is its own, and its programs' where it is one whose programs
	// share it; a program with none of its own takes its starter's.
	parents := map[string]map[string]bool{}
	roles := map[string]map[string]bool{}
	for _, r := range runs {
		context := ""
		if r.parent != nil {
			context = r.parent.context
		}
		purpose, inherit := execPurpose(r.argv, r.tool)
		switch {
		case purpose == "":
			purpose, r.context = context, context
		case inherit:
			r.context = purpose
		}
		if e := out.Exec[r.key]; e.Purpose == "" {
			e.Purpose = purpose
		}
		if parents[r.key] == nil {
			parents[r.key], roles[r.key] = map[string]bool{}, map[string]bool{}
		}
		parents[r.key][r.parentKey()] = true
		roles[r.key][map[bool]string{false: RoleHarness, true: RoleTool}[r.tool]] = true
	}
	for key, e := range out.Exec {
		sortWords(e.Phases, []string{PhaseStartup, PhaseTurn, PhaseExit})
		// Started by the build alone: no parents. Started by commands too: the build among them.
		if ps := parents[key]; len(ps) > 1 || !ps[""] {
			for p := range ps {
				if p != "" {
					e.Parents = append(e.Parents, p)
				}
			}
			slices.Sort(e.Parents)
			if ps[""] {
				e.Parents = append([]string{ExecBuild}, e.Parents...)
			}
		}
		switch r := roles[key]; {
		case r[RoleHarness] && r[RoleTool]:
			e.Role = RoleBoth
		case r[RoleTool]:
			e.Role = RoleTool
		default:
			e.Role = RoleHarness
		}
	}
	sortSocks := func(m map[string]*SockUse) {
		for _, s := range m {
			sortWords(s.Phases, []string{PhaseStartup, PhaseTurn, PhaseExit})
			slices.Sort(s.Programs)
			slices.Sort(s.Ops)
		}
	}
	sortSocks(out.Sockets.Listening)
	sortSocks(out.Sockets.Connections)
	sortSocks(out.Sockets.Unix)
	if out.MacOS != nil {
		sortSocks(out.MacOS.XPC)
		sortSocks(out.MacOS.LaunchItems)
		sortSocks(out.MacOS.TCC)
		sortSocks(out.MacOS.TaskPorts)
	}
	return out
}

// traceOp is what ev did to its path, as a path's op: "" for nothing to record.
func traceOp(ev TraceEvent, seenAbsent map[string]bool) string {
	switch ev.Err {
	case "":
	case "ENOENT", "ENOTDIR":
		seenAbsent[ev.Path] = true
		return "probe"
	case "EACCES", "EPERM", "EROFS":
		return "denied"
	case "EEXIST":
		return "stat"
	default:
		return ""
	}
	switch ev.Kind {
	case "open":
		switch {
		case ev.Create && seenAbsent[ev.Path]:
			delete(seenAbsent, ev.Path)
			return "create"
		case ev.Write:
			return "write"
		}
		return "read"
	case "list", "read", "write", "chdir":
		return ev.Kind
	case "stat":
		return "stat"
	case "lookup":
		return "probe"
	case "mkdir", "link", "create":
		return "create"
	case "unlink", "rmdir":
		return "delete"
	case "rename":
		return "rename"
	}
	return ""
}

// traceTree is the process tree under a root, from the forks the trace saw and the parents
// its events name.
type processTree struct {
	root    int
	parent  map[int]int
	thread  map[int]bool
	members map[int]bool
	all     bool // every process traced is a member
}

func traceTree(events []TraceEvent, root int) processTree {
	t := processTree{root: root, parent: map[int]int{}, thread: map[int]bool{}, members: map[int]bool{root: true}}
	for _, ev := range events {
		if ev.Kind == "fork" && ev.Child != 0 && ev.Child != ev.Pid {
			if _, ok := t.parent[ev.Child]; !ok {
				t.parent[ev.Child] = ev.Pid
				t.thread[ev.Child] = ev.Thread
			}
		}
		if ev.Ppid != 0 && ev.Ppid != ev.Pid {
			if _, ok := t.parent[ev.Pid]; !ok && ev.Pid != root {
				t.parent[ev.Pid] = ev.Ppid
			}
		}
	}
	// Membership by descent: a pid is in the tree where its chain of parents reaches the root.
	var in func(pid int, depth int) bool
	in = func(pid int, depth int) bool {
		if pid == root {
			return true
		}
		parent, ok := t.parent[pid]
		if !ok || depth > 64 {
			return false
		}
		return in(parent, depth+1)
	}
	for pid := range t.parent {
		if in(pid, 0) {
			t.members[pid] = true
		}
	}
	return t
}

func (t processTree) in(pid int) bool { return t.all || t.members[pid] }

// harness is the build's name, for its own processes.
func (s TraceScope) harness() string {
	for _, p := range s.Self {
		if b := filepath.Base(p); b == "claude" || b == "codex" {
			return b
		}
	}
	if len(s.Self) > 0 {
		return filepath.Base(s.Self[0])
	}
	return "harness"
}

// programName is the name of the program ev started: its argv[0]'s base name, as ps shows it
// (Codex runs its sandbox helper as itself under another name).
func programName(ev TraceEvent, exe string) string {
	if len(ev.Argv) > 0 && ev.Argv[0] != "" && ev.Argv[0] != "/proc/self/exe" {
		return filepath.Base(ev.Argv[0])
	}
	return filepath.Base(exe)
}

func (s TraceScope) isSelf(path string) bool {
	for _, p := range s.Self {
		if path == p || strings.HasPrefix(path, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// isHook reports an exec of the hook recorder: its executable, or a command line that names it
// or its folder (the shell a build runs a hook's command in).
func (s TraceScope) isHook(exe string, argv []string) bool {
	for _, h := range s.Hooks {
		if h == "" {
			continue
		}
		if exe == h || slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, h) }) {
			return true
		}
	}
	return false
}

// isTool reports a command line that runs one of the tool calls' commands: as an argument of
// its own (a shell's -c, a sandbox's command), or quoted in one for a shell (Claude Code's
// `eval '…'`).
func (s TraceScope) isTool(argv []string) bool {
	for _, t := range s.Tools {
		if t == "" {
			continue
		}
		forms := []string{t, strings.ReplaceAll(t, "'", `'\''`), strings.ReplaceAll(t, "'", `'"'"'`)}
		if slices.ContainsFunc(argv, func(a string) bool {
			return slices.ContainsFunc(forms, func(f string) bool { return strings.Contains(a, f) })
		}) {
			return true
		}
	}
	return false
}

// systemFiles are the files every process on the system reads through its C library or
// runtime: the resolver's, the user database, time zones, locales, certificate authorities,
// terminal descriptions. A build reading them says nothing of the build.
var systemFiles = []string{
	"/etc/nsswitch.conf", "/etc/host.conf", "/etc/hosts", "/etc/resolv.conf", "/etc/gai.conf", "/etc/passwd",
	"/etc/group", "/etc/localtime", "/etc/timezone", "/etc/services", "/etc/protocols", "/etc/mime.types",
	"/etc/ssl/", "/etc/pki/", "/etc/selinux/", "/selinux", "/usr/share/zoneinfo/", "/usr/share/locale/",
	"/usr/share/i18n/", "/usr/share/ca-certificates/", "/usr/share/terminfo/", "/etc/terminfo/", "/lib/terminfo/",
	"/etc/inputrc", "/usr/share/icu/", "/etc/ld.so.", "/etc/localtime",
	// macOS: /etc is /private/etc; the frameworks' own data.
	"/private/etc/hosts", "/private/etc/resolv.conf", "/private/etc/passwd", "/private/etc/group",
	"/private/etc/localtime", "/private/etc/services", "/private/etc/protocols", "/private/etc/ssl/",
	"/private/var/db/timezone/", "/var/db/timezone/", "/private/var/run/resolv.conf", "/usr/share/",
	"/Library/Preferences/.GlobalPreferences.plist", "/Library/Preferences/com.apple.", "/AppleInternal",
	"/private/var/db/mds/", "/private/var/db/DetachedSignatures", "/private/var/folders/zz/",
}

// libraryDirs hold shared libraries and the loader's files.
var libraryDirs = []string{"/lib/", "/lib64/", "/usr/lib/", "/usr/lib64/", "/usr/libexec/", "/usr/local/lib/",
	"/System/", "/Library/Apple/", "/private/var/db/dyld/", "/usr/lib/dyld", "/Library/Frameworks/"}

// dropReason is why path is not the build's to record, "" where it is.
func (s TraceScope) dropReason(path string) string {
	switch {
	case path == "/proc" || strings.HasPrefix(path, "/proc/"):
		return "proc"
	case path == "/sys" || strings.HasPrefix(path, "/sys/"):
		return "sys"
	case path == "/dev" || strings.HasPrefix(path, "/dev/"):
		return "dev"
	case path == "/newroot" || path == "/oldroot" || strings.HasPrefix(path, "/newroot/") || strings.HasPrefix(path, "/oldroot/"):
		return "sandbox-setup" // bubblewrap's new root and the old one, as it builds a sandbox
	case s.isSelf(path):
		return "self"
	}
	for _, h := range s.Hooks {
		if h != "" && (path == h || strings.HasPrefix(path, strings.TrimSuffix(h, "/")+"/")) {
			return "hooks"
		}
	}
	for _, t := range s.Test {
		if path == t || strings.HasPrefix(path, strings.TrimSuffix(t, "/")+"/") {
			return "test"
		}
	}
	if slices.Contains(s.TestWay, path) {
		return "test"
	}
	// What a package manager installed is the machine's: a build that looks for npm or pnpm
	// finds whatever versions this one has.
	for _, d := range installedDirs {
		if strings.HasPrefix(path, d) {
			return "machine"
		}
	}
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.") || strings.HasSuffix(base, ".dylib") {
		return "libraries"
	}
	for _, d := range libraryDirs {
		if strings.HasPrefix(path, d) {
			return "libraries"
		}
	}
	for _, f := range systemFiles {
		if path == strings.TrimSuffix(f, "/") || strings.HasPrefix(path, f) {
			return "system"
		}
	}
	return ""
}

// addWord is words with w, once.
func addWord(words []string, w string) []string {
	if slices.Contains(words, w) {
		return words
	}
	return append(words, w)
}

// sortWords orders words as order lists them.
func sortWords(words, order []string) {
	slices.SortFunc(words, func(a, b string) int { return cmp.Compare(slices.Index(order, a), slices.Index(order, b)) })
}
