package e2e

import (
	"cmp"
	"maps"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The files profile: what a harness build does on the file system, and which programs and
// sockets it uses, recorded by tracing its process tree while it runs a turn. Linux traces
// with strace (filetrace_strace.go), macOS with Endpoint Security's eslogger and lsof
// (filetrace_eslogger.go); each tracer's output becomes the same TraceEvents, and FilesOf
// makes the published shape of them: every path the build read, wrote, created, deleted,
// renamed or looked for, with its run's directories in placeholders; every program it started;
// whether it touched the decoy credentials the scenario planted (decoys.go); the sockets it
// listened on and connected to. What is not the build's is dropped and counted (dropReason):
// the kernel's files, shared libraries and the system's own files every process reads, the
// build's installation, terma's hooks past their start, the test's own files, the PATH
// searched (Lookups says for what), what a program the build starts does outside the run's own
// places, bubblewrap building a sandbox's root. reference/SCHEMA.md says the same for readers.

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
	// Terma are terma's executables (the recording shim, the binary): a process that runs one
	// is recorded as started, and nothing it does after.
	Terma []string
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
}

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

var (
	shortDigitsRE = regexp.MustCompile(`\d{2,}`)
	randomTokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)
	// pathTokenRE is a path in a command line: from a slash, or a placeholder and a slash.
	pathTokenRE = regexp.MustCompile(`(?:<[a-z-]+>)?(?:/[^\s"'=:;|&()<>]+)+`)
)

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
	terma   bool   // terma's, or started by terma's
	execs   int
	argv    []string // the command line it was started with
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
			if p.terma {
				out.Dropped["terma"]++
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
			own := scope.isSelf(exe)
			argv := scope.normalizeArgv(ev.Argv, ev.Path, own)
			key := strings.Join(argv, " ")

			e := out.Exec[key]
			if e == nil {
				e = &ExecUse{Argv: argv, By: by}
				out.Exec[key] = e
			}
			e.Count++
			e.Phases = addWord(e.Phases, phase)
			np := &traceProc{program: programName(ev, exe), exe: exe, execs: p.execs + 1, own: own, argv: ev.Argv}
			if np.own {
				np.program = scope.harness()
			}
			// terma, or the recording shim as a shell script's argument (macOS logs a script's
			// exec as its interpreter's).
			np.terma = scope.isTerma(ev.Path) || len(ev.Argv) > 1 && scope.isTerma(ev.Argv[1]) && isShell(ev.Argv[0])
			if tree.thread[ev.Pid] {
				*p = *np
			} else {
				procs[ev.Pid] = np
			}
			continue
		}
		if p.terma {
			out.Dropped["terma"]++
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
	for _, e := range out.Exec {
		sortWords(e.Phases, []string{PhaseStartup, PhaseTurn, PhaseExit})
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

func (s TraceScope) isTerma(path string) bool {
	return slices.Contains(s.Terma, path)
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

// normalize is path with the run's literals in placeholders, and what varies by itself (ids,
// timestamps, process ids in a temporary file's name) in placeholders of its kind. A path inside
// a sandbox's new root as bubblewrap builds it (/newroot, /oldroot) is normalized below it.
func (s TraceScope) normalize(path string) string {
	for _, root := range []string{"/newroot", "/oldroot"} {
		if rest, ok := strings.CutPrefix(path, root+"/"); ok {
			if n := s.normalize("/" + rest); strings.HasPrefix(n, "<") {
				return root + "/" + n
			} else {
				return root + n
			}
		}
	}
	for _, lit := range s.literals() {
		if path == lit || strings.HasPrefix(path, lit+"/") {
			path = s.Replace[lit] + strings.TrimPrefix(path, lit)
			break
		}
	}
	return s.normalizeSegments(path)
}

// literals are the scope's literals, longest first.
func (s TraceScope) literals() []string {
	return slices.DeleteFunc(slices.SortedFunc(maps.Keys(s.Replace), func(a, b string) int { return cmp.Compare(len(b), len(a)) }),
		func(lit string) bool { return lit == "" })
}

// normalizeSegments normalizes each segment of a path whose literals are replaced: a literal's
// slug (Claude Code names a project's folder by its path, every character but letters and
// digits a dash) as <name-slug>, and the values a run makes.
func (s TraceScope) normalizeSegments(path string) string {
	segs := strings.Split(path, "/")
	tmp := false
	for i, seg := range segs {
		if seg == "<tmp>" || seg == "tmp" || seg == ".tmp" || seg == "temp" {
			tmp = true
		}
		if strings.HasPrefix(seg, "<") && strings.HasSuffix(seg, ">") && strings.Count(seg, "<") == 1 {
			continue
		}
		for _, lit := range s.literals() {
			if len(lit) < 8 {
				continue
			}
			if slug := nonAlnumRE.ReplaceAllString(lit, "-"); strings.Contains(seg, slug) {
				seg = strings.ReplaceAll(seg, slug, strings.TrimSuffix(s.Replace[lit], ">")+"-slug>")
			}
		}
		// Git's loose objects sit in folders named by the first two digits of an id.
		if i > 0 && segs[i-1] == "objects" && i > 1 && segs[i-2] == ".git" && gitFanoutRE.MatchString(seg) {
			segs[i] = "<id>"
			continue
		}
		segs[i] = volatileSegment(seg, tmp)
	}
	return strings.Join(segs, "/")
}

var (
	nonAlnumRE  = regexp.MustCompile(`[^A-Za-z0-9]`)
	gitFanoutRE = regexp.MustCompile(`^[0-9a-f]{2}$`)
	// tempNameRE is a temporary file's random suffix: Rust's tempfile (.tmpXXXXXX), mktemp's
	// (tmp.XXXXXXXXXX), mkdtemp's after a dash (name-XXXXXX).
	tempNameRE = regexp.MustCompile(`(\.tmp|^tmp\.|\.tmp\.)([A-Za-z0-9]{6,10})$`)
	dashTempRE = regexp.MustCompile(`-([A-Za-z0-9]{6})$`)
	// hexTokenRE is a short random hex token, as a temporary name has one (claude-3f2a-cwd).
	hexTokenRE = regexp.MustCompile(`^[0-9a-f]{4,12}$`)
)

// volatileSegment is one path segment with its run's own values in placeholders. A segment
// with one (a temporary file's), or one in the temporary folder, has its other numbers and
// random tokens too: a process id beside a time.
func volatileSegment(seg string, inTmp bool) string {
	if seg == "" || seg == "." || seg == ".." {
		return seg
	}
	// A number alone, or a number's file (a process's: 56.json), is the run's.
	if digitStemRE.MatchString(seg) {
		return digitStemRE.ReplaceAllString(seg, "<n>$1")
	}
	was := seg
	seg = uuidRE.ReplaceAllString(seg, "<uuid>")
	seg = datetimeRE.ReplaceAllString(seg, "<time>")
	seg = hexRE.ReplaceAllString(seg, "<hash>")
	seg = tempNameRE.ReplaceAllString(seg, "$1<id>")
	// A file in the temporary folder named by a short random token alone, letters only now and
	// then (Claude Code's task output, tasks/bkjuwxtnl.output).
	if inTmp {
		seg = tmpTokenStemRE.ReplaceAllString(seg, "<id>$1")
	}
	if inTmp {
		seg = dashTempRE.ReplaceAllStringFunc(seg, func(m string) string {
			// mkdtemp's letters and digits, all of one case now and then (fswatch-probe-WJPDWS).
			if mixedToken(m[1:]) || strings.Trim(m[1:], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
				return "-<id>"
			}
			return m
		})
	}
	// A key's kind (id_ed25519) is no number.
	seg = strings.ReplaceAll(seg, "ed25519", "\x00")
	if seg == was && !inTmp && !strings.Contains(seg, ".tmp") && !strings.HasPrefix(seg, "tmp") && !digitsRE.MatchString(seg) {
		return strings.ReplaceAll(seg, "\x00", "ed25519")
	}
	// A token at a time: a number, letters and digits mixed, a short hex token in the
	// temporary folder.
	seg = randomTokenRE.ReplaceAllStringFunc(seg, func(tok string) string {
		switch {
		case inTmp && hexTokenRE.MatchString(tok):
			// In the temporary folder, a short hex token is random, digits only or not.
			return "<id>"
		case strings.Trim(tok, "0123456789") == "":
			if len(tok) >= 2 {
				return "<n>"
			}
		case len(tok) >= 4 && strings.ContainsAny(tok, "0123456789"), inTmp && len(tok) >= 6 && mixedToken(tok):
			return "<id>"
		}
		return tok
	})
	// A short token after one of the run's values is the run's too, whatever its letters
	// (Claude Code's shell snapshot, snapshot-zsh-<n>-rbqfzy.sh).
	seg = afterRunValueRE.ReplaceAllString(seg, "$1-<id>")
	return strings.ReplaceAll(seg, "\x00", "ed25519")
}

// installedDirs are where package managers install programs.
var installedDirs = []string{"/opt/homebrew/", "/usr/local/Cellar/", "/usr/local/lib/node_modules/", "/usr/local/opt/", "/home/linuxbrew/"}

// mkdtempRE is a path ending in a dash and six letters or digits, as mkdtemp names a folder.
var mkdtempRE = regexp.MustCompile(`/[^/]*-[A-Za-z0-9]{6}$`)

// tmpTokenStemRE is a file name that is a short lower-case token and an extension.
var tmpTokenStemRE = regexp.MustCompile(`^[a-z0-9]{8,12}(\.[a-z]+)$`)

// afterRunValueRE is a placeholder, a dash and a six-character token.
var afterRunValueRE = regexp.MustCompile(`(<(?:n|id|hash|uuid|time)>)-[A-Za-z0-9]{6}\b`)

// digitStemRE is a segment that is a number, or a number and an extension.
var digitStemRE = regexp.MustCompile(`^\d+((?:\.[A-Za-z]+)*)$`)

// mixedToken reports a token a program made at random: letters of both cases, or letters and
// digits.
func mixedToken(tok string) bool {
	lower := strings.IndexFunc(tok, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0
	upper := strings.IndexFunc(tok, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0
	digit := strings.ContainsAny(tok, "0123456789")
	return lower && upper || (lower || upper) && digit
}

// normalizeArgv is a command line with its paths and the run's values in placeholders, terma
// by its name. The build's own helper processes take numbers (descriptors, process ids) the
// run picks: those are <n>.
func (s TraceScope) normalizeArgv(argv []string, exe string, own bool) []string {
	if len(argv) == 0 {
		argv = []string{exe}
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = s.normalizeText(a)
		// A number the run picks: a helper's descriptors, a process id it asks about.
		if i > 0 && a != "" && strings.Trim(a, "0123456789") == "" && (own || len(a) >= 4) {
			out[i] = "<n>"
		}
	}
	if slices.ContainsFunc(s.Terma, func(p string) bool { return p == argv[0] || p == exe }) {
		out[0] = "terma"
	}
	if len(argv) > 1 && isShell(argv[0]) && s.isTerma(argv[1]) {
		out = append([]string{"terma"}, out[2:]...)
	}
	return out
}

// normalizeText is a string with every path in it normalized: an argument may hold several
// (a shell's command).
func (s TraceScope) normalizeText(text string) string {
	if len(s.Path) > 1 {
		text = strings.ReplaceAll(text, s.Path, "<path>")
	}
	for _, t := range s.Terma {
		text = strings.ReplaceAll(text, t, "terma")
	}
	for _, lit := range s.literals() {
		text = strings.ReplaceAll(text, lit, s.Replace[lit])
	}
	for lit, name := range s.Endpoints {
		text = strings.ReplaceAll(text, lit, name)
	}
	text = pathTokenRE.ReplaceAllStringFunc(text, s.normalizeSegments)
	// An id the run made, in a name (Claude Code's keychain entry is named by a hash of its
	// configuration folder).
	text = uuidRE.ReplaceAllString(text, "<uuid>")
	// A heredoc's delimiter the run picks at random (Claude Code's shell snapshot:
	// `<< 'PATH_END_c3ph6ooejs'`).
	text = heredocTagRE.ReplaceAllString(text, "${1}_<id>")
	return argHexRE.ReplaceAllStringFunc(text, func(tok string) string {
		if strings.ContainsAny(tok, "0123456789") {
			return "<hash>"
		}
		return tok
	})
}

// argHexRE is a hex token in a command line, eight digits or more.
var argHexRE = regexp.MustCompile(`\b[0-9a-f]{8,}\b`)

// heredocTagRE is an upper-case word with a random lower-case suffix: a heredoc delimiter a
// program made unique.
var heredocTagRE = regexp.MustCompile(`\b([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*)_[a-z0-9]{6,16}\b`)

// socketEvent records a socket call.
func (s TraceScope) socketEvent(out *FilesProbe, ev TraceEvent, phase, program string, dns map[string]bool) {
	if ev.Kind == "dns" {
		if ev.Name != "" {
			dns[strings.TrimSuffix(strings.ToLower(ev.Name), ".")] = true
		}
		return
	}
	sock := ev.Sock
	if sock == nil {
		return
	}
	use := func(m map[string]*SockUse, key, role string) *SockUse {
		u := m[key]
		if u == nil {
			u = &SockUse{Role: role, Missing: true}
			m[key] = u
		}
		u.Count++
		u.Phases = addWord(u.Phases, phase)
		if program != "" {
			u.Programs = addWord(u.Programs, program)
		}
		return u
	}
	if sock.Proto == "unix" {
		path := sock.Host
		if path == "" {
			return
		}
		key := path
		if !strings.HasPrefix(path, "@") {
			key = s.normalize(path)
		}
		u := use(out.Sockets.Unix, key, s.unixRole(path))
		u.Ops = addWord(u.Ops, ev.Kind)
		if ev.Err != "ENOENT" && ev.Err != "ECONNREFUSED" {
			u.Missing = false
		}
		if ev.Kind == "connect" && ev.Err == "" && slices.Contains(s.Decoys, path) {
			raiseDecoy(out, s.normalize(path), "connected")
		}
		return
	}
	switch ev.Kind {
	case "connect":
		if sock.Host == "" || sock.Port == 0 && sock.Proto == "udp" {
			return
		}
		to, role := s.endpoint(sock.Host, sock.Port)
		u := use(out.Sockets.Connections, sock.Proto+" "+to, role)
		if ev.Err != "ENOENT" && ev.Err != "ECONNREFUSED" && ev.Err != "ENETUNREACH" {
			u.Missing = false
		}
	case "bind", "listen":
		// A client's own end (a UDP socket bound to any port to send from) listens for nothing.
		if ev.Kind == "bind" && (sock.Proto == "tcp" || sock.Ephemeral || sock.Port == 0) {
			return
		}
		port := strconv.Itoa(sock.Port)
		if sock.Ephemeral {
			port = "<ephemeral>"
		}
		host := sock.Host
		if host == "" {
			host = "*"
		}
		u := use(out.Sockets.Listening, sock.Proto+" "+net.JoinHostPort(host, port), "")
		u.Missing = false
	}
}

// endpoint names a socket address: the run's own by their names, the resolver, a loopback
// address as <local>, any other as <remote>, each with its port.
func (s TraceScope) endpoint(host string, port int) (string, string) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if name, ok := s.Endpoints[addr]; ok {
		return name, strings.Trim(name, "<>")
	}
	ip := net.ParseIP(host)
	switch {
	case port == 53:
		return "<resolver>:53", "dns"
	case ip != nil && ip.IsLoopback():
		return "<local>:" + strconv.Itoa(port), ""
	}
	return "<remote>:" + strconv.Itoa(port), ""
}

// unixRole says what a Unix socket is, where its path says: the planted decoys, an SSH agent,
// Docker, D-Bus, X11, Wayland, the resolver cache, the system log.
func (s TraceScope) unixRole(path string) string {
	base := filepath.Base(path)
	switch {
	case slices.Contains(s.Decoys, path) && strings.Contains(path, "docker"):
		return "decoy docker"
	case slices.Contains(s.Decoys, path):
		return "decoy ssh-agent"
	case strings.Contains(path, "ssh-agent") || strings.Contains(path, "agent.") && strings.Contains(path, "ssh") || strings.Contains(path, "com.apple.launchd") && strings.HasSuffix(path, "Listeners"):
		return "ssh-agent"
	case strings.Contains(base, "docker") || strings.Contains(path, "/.docker/") || strings.Contains(path, "podman"):
		return "docker"
	case strings.Contains(path, "dbus") || base == "bus":
		return "dbus"
	case strings.Contains(path, ".X11-unix"):
		return "x11"
	case strings.HasPrefix(base, "wayland-"):
		return "wayland"
	case strings.Contains(path, "nscd"):
		return "nscd"
	case path == "/dev/log" || strings.Contains(path, "journal") || strings.Contains(path, "syslog"):
		return "syslog"
	case strings.HasPrefix(path, "@"):
		return "abstract"
	}
	return ""
}

// macEvent records what only Endpoint Security sees.
func (s TraceScope) macEvent(out *FilesProbe, ev TraceEvent, phase, program string) {
	if out.MacOS == nil {
		out.MacOS = &MacOSUse{XPC: map[string]*SockUse{}, LaunchItems: map[string]*SockUse{}, TCC: map[string]*SockUse{}, TaskPorts: map[string]*SockUse{}}
	}
	m := map[string]map[string]*SockUse{"xpc": out.MacOS.XPC, "launch-item": out.MacOS.LaunchItems, "tcc": out.MacOS.TCC, "task-port": out.MacOS.TaskPorts}[ev.Kind]
	name := ev.Name
	switch {
	case ev.Kind == "task-port" && program != "":
		// A helper that reads every process (ps) does so by its nature: one entry, and no list
		// of what the machine runs.
		name = "<processes>"
	case ev.Kind == "task-port":
		name = filepath.Base(name) // the process, not where this machine installed it
	case strings.HasPrefix(name, "/"):
		name = s.normalize(name)
	}
	if name == "" || name == "." {
		return
	}
	u := m[name]
	if u == nil {
		u = &SockUse{}
		m[name] = u
	}
	u.Count++
	u.Phases = addWord(u.Phases, phase)
	if program != "" {
		u.Programs = addWord(u.Programs, program)
	}
}

// decoyEvent raises a decoy's level with what ev did to it.
func (s TraceScope) decoyEvent(out *FilesProbe, ev TraceEvent, op string) {
	if !slices.Contains(s.Decoys, ev.Path) {
		return
	}
	level := map[string]string{"probe": "stat", "stat": "stat", "list": "stat", "denied": "stat", "chdir": "stat",
		"write": "written", "create": "written", "delete": "deleted", "rename": "deleted"}[op]
	switch {
	case op == "read" && ev.Kind == "read":
		level = "read"
	case op == "read":
		level = "opened"
	}
	if level != "" {
		raiseDecoy(out, s.normalize(ev.Path), level)
	}
}

func raiseDecoy(out *FilesProbe, key, level string) {
	if slices.Index(decoyLevels, level) > slices.Index(decoyLevels, out.Decoys[key]) {
		out.Decoys[key] = level
	}
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

// isShell reports a shell's path or name.
func isShell(argv0 string) bool {
	switch filepath.Base(argv0) {
	case "sh", "bash", "zsh", "dash", "fish", "ksh":
		return true
	}
	return false
}
