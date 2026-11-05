package e2e

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A trace of a build's run as strace -f -ttt -yy writes one: the build (pid 100) starts, a
// thread of it (101) reads its state, it starts a shell (102) that runs git, a call is
// interrupted by another process and resumed, it looks for a file that is not there, renames
// its state into place, opens a decoy, reads a decoy, lists a decoy folder, connects to the
// provider, the resolver, a decoy socket and a Unix socket that is not there, and listens on
// an ephemeral port. terma's hook (103) runs, and reads what is its own.
const straceFixture = `100   1000.000100 execve("/opt/claude/2.1.318/claude", ["claude", "-p", "Say hello."], 0xffff /* 20 vars */) = 0
100   1000.000200 openat(AT_FDCWD</sb/repo>, "/lib/aarch64-linux-gnu/libc.so.6", O_RDONLY|O_CLOEXEC) = 3</lib/aarch64-linux-gnu/libc.so.6>
100   1000.000300 openat(AT_FDCWD</sb/repo>, "/proc/self/maps", O_RDONLY) = 3</proc/self/maps>
100   1000.000400 clone(child_stack=0xffff, flags=CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_THREAD|CLONE_SYSVSEM, parent_tid=[101], tls=0xffff) = 101
101   1000.000500 openat(AT_FDCWD</sb/repo>, "/sb/claude/.claude.json", O_RDONLY|O_CLOEXEC) = 4</sb/claude/.claude.json>
101   1000.000600 read(4</sb/claude/.claude.json>, "{\"numStartups\":5}", 4096) = 17
100   1000.000700 newfstatat(AT_FDCWD</sb/repo>, "CLAUDE.md", 0xffff, 0) = -1 ENOENT (No such file or directory)
100   1000.000800 statx(AT_FDCWD</sb/repo>, "/sb/home/.ssh/id_ed25519", AT_STATX_SYNC_AS_STAT, STATX_ALL, 0xffff) = 0
100   1000.000900 openat(AT_FDCWD</sb/repo>, "/sb/home/.aws/credentials", O_RDONLY|O_CLOEXEC <unfinished ...>
101   1000.001000 newfstatat(AT_FDCWD</sb/repo>, "/usr/local/bin/rg", 0xffff, 0) = -1 ENOENT (No such file or directory)
100   1000.001100 <... openat resumed>) = 5</sb/home/.aws/credentials>
100   1000.001200 openat(AT_FDCWD</sb/repo>, "/sb/home/.netrc", O_RDONLY|O_CLOEXEC) = 6</sb/home/.netrc>
100   1000.001300 read(6</sb/home/.netrc>, "machine x\n", 4096) = 10
100   1000.001400 openat(AT_FDCWD</sb/repo>, "/sb/home/.ssh", O_RDONLY|O_NONBLOCK|O_CLOEXEC|O_DIRECTORY) = 7</sb/home/.ssh>
100   1000.001500 socket(AF_INET, SOCK_STREAM|SOCK_CLOEXEC|SOCK_NONBLOCK, IPPROTO_IP) = 8<TCP:[3001]>
100   1000.001600 connect(8<TCP:[3001]>, {sa_family=AF_INET, sin_port=htons(43111), sin_addr=inet_addr("127.0.0.1")}, 16) = -1 EINPROGRESS (Operation now in progress)
100   2000.000100 openat(AT_FDCWD</sb/repo>, "/sb/claude/.claude.json.tmp.100.1793845316229", O_WRONLY|O_CREAT|O_EXCL|O_CLOEXEC, 0600) = 9</sb/claude/.claude.json.tmp.100.1793845316229>
100   2000.000200 rename("/sb/claude/.claude.json.tmp.100.1793845316229", "/sb/claude/.claude.json") = 0
100   2000.000300 clone(child_stack=NULL, flags=CLONE_CHILD_CLEARTID|CLONE_CHILD_SETTID|SIGCHLD <unfinished ...>
102   2000.000400 execve("/usr/bin/bash", ["/usr/bin/bash", "-c", "-l", "source /sb/claude/shell-snapshots/snapshot-bash-1793845316229-ab12cd.sh && eval 'git status --short' && pwd -P >| /tmp/claude-3f2a-cwd"], 0xffff /* 20 vars */ <unfinished ...>
100   2000.000500 <... clone resumed>) = 102
102   2000.000600 <... execve resumed>) = 0
102   2000.000700 openat(AT_FDCWD</sb/repo>, "/sb/home/.bashrc", O_RDONLY) = -1 ENOENT (No such file or directory)
102   2000.000800 clone(child_stack=NULL, flags=CLONE_CHILD_CLEARTID|CLONE_CHILD_SETTID|SIGCHLD, child_tidptr=0xffff) = 104
104   2000.000900 execve("/usr/local/sbin/git", ["git", "status", "--short"], 0xffff /* 20 vars */) = -1 ENOENT (No such file or directory)
104   2000.001000 execve("/usr/bin/git", ["git", "status", "--short"], 0xffff /* 20 vars */) = 0
104   2000.001100 openat(AT_FDCWD</sb/repo>, ".git/index", O_RDONLY) = 3</sb/repo/.git/index>
104   2000.001200 openat(AT_FDCWD</sb/repo>, "/sb/home/.gitconfig", O_RDONLY) = -1 ENOENT (No such file or directory)
104   2000.001300 openat(AT_FDCWD</sb/repo>, "/etc/passwd", O_RDONLY|O_CLOEXEC) = 3</etc/passwd>
100   2000.001400 socket(AF_UNIX, SOCK_STREAM|SOCK_CLOEXEC|SOCK_NONBLOCK, 0) = 10<UNIX-STREAM:[3002]>
100   2000.001500 connect(10<UNIX-STREAM:[3002]>, {sa_family=AF_UNIX, sun_path="/sb/home/.ssh/agent.sock"}, 110) = 0
100   2000.001600 connect(11<UNIX-STREAM:[3003]>, {sa_family=AF_UNIX, sun_path="/var/run/nscd/socket"}, 110) = -1 ENOENT (No such file or directory)
100   2000.001700 connect(12<UDP:[3004]>, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("127.0.0.11")}, 16) = 0
100   2000.001800 sendto(12<UDP:[172.17.0.2:47530->127.0.0.11:53]>, "\354\303\1\0\0\1\0\0\0\0\0\0\3api\tanthropic\3com\0\0\1\0\1", 33, MSG_NOSIGNAL, NULL, 0) = 33
100   2000.001900 sendmmsg(12<UDP:[172.17.0.2:47530->127.0.0.11:53]>, [{msg_hdr={msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="\1\2\1\0\0\1\0\0\0\0\0\0\7statsig\3com\0\0\34\0\1", iov_len=29}], msg_iovlen=1, msg_controllen=0, msg_flags=0}, msg_len=29}], 1, MSG_NOSIGNAL) = 1
100   2000.002000 bind(13<TCP:[3005]>, {sa_family=AF_INET, sin_port=htons(0), sin_addr=inet_addr("127.0.0.1")}, 16) = 0
100   2000.002100 listen(13<TCP:[127.0.0.1:42087]>, 511) = 0
100   2000.002200 bind(14<TCP:[3006]>, {sa_family=AF_INET, sin_port=htons(7777), sin_addr=inet_addr("0.0.0.0")}, 16) = 0
100   2000.002300 listen(14<TCP:[0.0.0.0:7777]>, 511) = 0
100   2000.002400 clone3({flags=CLONE_VM|CLONE_VFORK, exit_signal=SIGCHLD, stack=0xffff, stack_size=0x9000}, 88) = 103
103   2000.002500 execve("/sb/path/terma", ["/sb/path/terma", "hook", "pre-tool-use"], 0xffff /* 20 vars */) = 0
103   2000.002600 openat(AT_FDCWD</sb/repo>, "/sb/terma/config.json", O_RDONLY) = 3</sb/terma/config.json>
103   2000.002700 openat(AT_FDCWD</sb/repo>, "/sb/home/.aws/credentials", O_RDONLY) = 3</sb/home/.aws/credentials>
103   2000.002800 +++ exited with 0 +++
100   3000.000100 unlinkat(AT_FDCWD</sb/repo>, "/sb/claude/session.lock", 0) = 0
100   3000.000200 mkdir("/sb/claude/todos", 0700) = -1 EEXIST (File exists)
100   3000.000300 exit_group(0)              = ?
100   3000.000400 +++ exited with 0 +++
`

func straceScope() TraceScope {
	return TraceScope{
		Platform: "linux/amd64", Tracer: "strace",
		Replace: map[string]string{"/sb/repo": "<repo>", "/sb/home": "<home>", "/sb/claude": "<claude-config>", "/sb": "<sandbox>",
			"/sb/terma": "<terma-config>", "/opt/claude/2.1.318": "<install>", "/tmp": "<tmp>"},
		Self:      []string{"/opt/claude/2.1.318/claude", "/opt/claude/2.1.318", "/sb/path/claude"},
		Terma:     []string{"/sb/path/terma", "/opt/terma"},
		PathDirs:  []string{"/sb/path", "/usr/local/sbin", "/usr/local/bin", "/usr/bin", "/bin"},
		TurnStart: time.Unix(1500, 0), TurnEnd: time.Unix(2500, 0),
		Decoys:    []string{"/sb/home/.ssh/id_ed25519", "/sb/home/.aws/credentials", "/sb/home/.netrc", "/sb/home/.npmrc", "/sb/home/.ssh/agent.sock"},
		Endpoints: map[string]string{"127.0.0.1:43111": "<provider>", "127.0.0.1:43222": "<proxy>"},
	}
}

func TestParseStraceOffline(t *testing.T) {
	events, root := ParseStrace(strings.NewReader(straceFixture))
	if root != 100 {
		t.Fatalf("root = %d, want 100", root)
	}
	byKind := map[string]int{}
	for _, ev := range events {
		byKind[ev.Kind]++
	}
	// A resumed call is read whole, at the time it started.
	for _, ev := range events {
		if ev.Path == "/sb/home/.aws/credentials" && ev.Pid == 100 {
			if !ev.At.Equal(epochTime("1000.000900")) || ev.Kind != "open" || ev.Write {
				t.Errorf("resumed open = %+v", ev)
			}
		}
		if ev.Kind == "exec" && ev.Pid == 102 {
			if want := []string{"/usr/bin/bash", "-c", "-l"}; !reflect.DeepEqual(ev.Argv[:3], want) || ev.Err != "" {
				t.Errorf("resumed execve = %+v", ev)
			}
		}
		if ev.Kind == "fork" && ev.Child == 101 && !ev.Thread {
			t.Error("a CLONE_THREAD clone is a thread")
		}
		if ev.Kind == "fork" && ev.Child == 102 && ev.Thread {
			t.Error("a fork is a process")
		}
		// A relative path is the directory descriptor's.
		if ev.Pid == 104 && strings.HasSuffix(ev.Path, "index") && ev.Path != "/sb/repo/.git/index" {
			t.Errorf("relative path resolved to %q", ev.Path)
		}
	}
	for kind, want := range map[string]int{"exec": 5, "fork": 4, "rename": 1, "read": 2, "list": 1, "dns": 2, "listen": 2, "connect": 4} {
		if byKind[kind] != want {
			t.Errorf("%d %s events, want %d (%v)", byKind[kind], kind, want, byKind)
		}
	}
}

func TestFilesOfStraceOffline(t *testing.T) {
	events, root := ParseStrace(strings.NewReader(straceFixture))
	scope := straceScope()
	scope.Root = root
	p := FilesOf(events, scope)
	want := map[string]PathUse{
		"<claude-config>/.claude.json":             {Ops: []string{"read", "write"}, Phases: []string{"startup", "turn"}},
		"<claude-config>/.claude.json.tmp.<n>.<n>": {Ops: []string{"write", "rename"}, Phases: []string{"turn"}},
		"<repo>/CLAUDE.md":                         {Ops: []string{"probe"}, Phases: []string{"startup"}},
		"<home>/.ssh":                              {Ops: []string{"list"}, Phases: []string{"startup"}},
		"<home>/.bashrc":                           {Ops: []string{"probe"}, Phases: []string{"turn"}, Programs: []string{"bash"}},
		"<repo>/.git/index":                        {Ops: []string{"read"}, Phases: []string{"turn"}, Programs: []string{"git"}},
		"<home>/.gitconfig":                        {Ops: []string{"probe"}, Phases: []string{"turn"}, Programs: []string{"git"}},
		"<claude-config>/session.lock":             {Ops: []string{"delete"}, Phases: []string{"exit"}},
		"<claude-config>/todos":                    {Ops: []string{"stat"}, Phases: []string{"exit"}},
	}
	for path, w := range want {
		got := p.Paths[path]
		if got == nil {
			t.Errorf("no %s in %v", path, mapsKeys(p.Paths))
			continue
		}
		if !reflect.DeepEqual(*got, w) {
			t.Errorf("%s = %+v, want %+v", path, *got, w)
		}
	}
	// What is not the build's is dropped and counted: its libraries, /proc, its own folder,
	// the system's files, terma's hook past its start, the PATH searched.
	for _, path := range []string{"/lib/aarch64-linux-gnu/libc.so.6", "<terma-config>/config.json", "/etc/passwd", "/proc/self/maps", "/usr/local/bin/rg"} {
		if p.Paths[path] != nil {
			t.Errorf("%s recorded: %+v", path, p.Paths[path])
		}
	}
	for why, n := range map[string]int{"libraries": 1, "proc": 1, "terma": 2, "system": 1, "path-search": 2} {
		if p.Dropped[why] != n {
			t.Errorf("dropped[%s] = %d, want %d (%v)", why, p.Dropped[why], n, p.Dropped)
		}
	}
	if !reflect.DeepEqual(p.Lookups, []string{"rg"}) {
		t.Errorf("lookups = %v, want rg alone (a shell's search for git is the shell's)", p.Lookups)
	}
	// The programs it started, by their command lines: the shell's temporary names in
	// placeholders, the hook by terma's name; the scenario's own command is not one.
	bashCmd := "/usr/bin/bash -c -l source <claude-config>/shell-snapshots/snapshot-bash-<n>-<id>.sh && eval 'git status --short' && pwd -P >| <tmp>/claude-<id>-cwd"
	for key, w := range map[string]ExecUse{
		bashCmd:                   {Phases: []string{"turn"}, Count: 1, By: "claude"},
		"git status --short":      {Phases: []string{"turn"}, Count: 1, By: "bash"},
		"terma hook pre-tool-use": {Phases: []string{"turn"}, Count: 1, By: "claude"},
	} {
		got := p.Exec[key]
		if got == nil {
			t.Errorf("no exec %q in %v", key, mapsKeys(p.Exec))
			continue
		}
		if !reflect.DeepEqual(got.Phases, w.Phases) || got.Count != w.Count || got.By != w.By {
			t.Errorf("exec %q = %+v, want %+v", key, *got, w)
		}
	}
	if len(p.Exec) != 3 {
		t.Errorf("exec = %v", mapsKeys(p.Exec))
	}
	// Decoys: looked at, opened, read, connected to, and one left alone. terma's own read of
	// one is not the build's.
	for path, level := range map[string]string{"<home>/.ssh/id_ed25519": "stat", "<home>/.aws/credentials": "opened",
		"<home>/.netrc": "read", "<home>/.npmrc": "not touched", "<home>/.ssh/agent.sock": "connected"} {
		if p.Decoys[path] != level {
			t.Errorf("decoy %s = %q, want %q", path, p.Decoys[path], level)
		}
	}
	// Sockets: the provider by name, the resolver and what was asked of it, a missing Unix
	// socket, an ephemeral listener and a fixed one.
	if c := p.Sockets.Connections["tcp <provider>"]; c == nil || c.Role != "provider" || c.Missing {
		t.Errorf("provider connection = %+v (%v)", c, mapsKeys(p.Sockets.Connections))
	}
	if c := p.Sockets.Connections["udp <resolver>:53"]; c == nil || c.Role != "dns" {
		t.Errorf("resolver connection = %+v", c)
	}
	if !reflect.DeepEqual(p.Sockets.DNS, []string{"api.anthropic.com", "statsig.com"}) {
		t.Errorf("dns = %v", p.Sockets.DNS)
	}
	if u := p.Sockets.Unix["/var/run/nscd/socket"]; u == nil || !u.Missing || u.Role != "nscd" {
		t.Errorf("nscd = %+v", u)
	}
	if u := p.Sockets.Unix["<home>/.ssh/agent.sock"]; u == nil || u.Role != "decoy ssh-agent" || u.Missing {
		t.Errorf("decoy agent = %+v", u)
	}
	for _, key := range []string{"tcp 127.0.0.1:<ephemeral>", "tcp 0.0.0.0:7777"} {
		if p.Sockets.Listening[key] == nil {
			t.Errorf("no listener %s in %v", key, mapsKeys(p.Sockets.Listening))
		}
	}
	// The shape is stable: marshalled twice, the same bytes.
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(FilesOf(events, scope))
	if string(a) != string(b) {
		t.Error("FilesOf is not deterministic")
	}
}

func mapsKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestStraceParseDetailsOffline(t *testing.T) {
	if got := string(cUnescape(`a\tb\\c\"d\x41\101\0e`)); got != "a\tb\\c\"dAA\x00e" {
		t.Errorf("cUnescape = %q", got)
	}
	args, end := splitArgs(`1</dev/null<char 1:3>>, "a,b", {x=1, y=[2, 3]}, [28 => 16]) = 0`)
	if end < 0 || len(args) != 4 || args[1] != `"a,b"` {
		t.Errorf("splitArgs = %q, %d", args, end)
	}
	if a := annotation(`1</dev/null<char 1:3>>`); a != "/dev/null" {
		t.Errorf("annotation = %q", a)
	}
	proto, local, peer := socketAnnotation("TCPv6:[[::1]:5000->[::1]:443]")
	if proto != "tcp" || local != "[::1]:5000" || peer != "[::1]:443" {
		t.Errorf("socketAnnotation = %q %q %q", proto, local, peer)
	}
	if h, p := splitHostPort("[::1]:443"); h != "::1" || p != 443 {
		t.Errorf("splitHostPort = %q %d", h, p)
	}
	// A path whose directory is a descriptor of another folder; one before any working
	// directory was seen; a call's failure.
	events, _ := ParseStrace(strings.NewReader(`7 1.0 openat(5</a/b>, "c/d", O_RDONLY) = 3</a/b/c/d>
7 1.1 stat("rel", 0xffff) = -1 EACCES (Permission denied)
7 1.2 chdir("/x") = 0
7 1.3 access("y", F_OK) = 0
7 1.4 inotify_add_watch(3<anon_inode:inotify>, "/x/watched", IN_MODIFY) = 1
7 1.5 openat(AT_FDCWD</x>, "/repo", O_RDONLY|O_DIRECTORY|O_PATH) = 15</repo>
7 1.55 clone(child_stack=0xffff, flags=CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_THREAD) = 8
9 1.56 openat(AT_FDCWD</x>, "/elsewhere", O_RDONLY|O_DIRECTORY|O_PATH) = 15</elsewhere>
8 1.6 openat(AT_FDCWD</x>, "/proc/self/fd/15/notes.txt.tmp.54.c64e", O_WRONLY|O_CREAT|O_EXCL, 0666) = 17</repo/notes.txt.tmp.54.c64e>
8 1.7 renameat2(AT_FDCWD</x>, "/proc/self/fd/15/notes.txt.tmp.54.c64e", AT_FDCWD</x>, "/proc/self/fd/15/notes.txt", 0) = 0
`))
	// A thread uses its process's descriptors, not another process's of the same number.
	want := []string{"/a/b/c/d", "rel", "/x", "/x/y", "/x/watched", "/repo", "", "/elsewhere", "/repo/notes.txt.tmp.54.c64e", "/repo/notes.txt.tmp.54.c64e"}
	if last := events[len(events)-1]; last.To != "/repo/notes.txt" {
		t.Errorf("a rename through a descriptor's folder went to %q", last.To)
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v", events)
	}
	for i, w := range want {
		if events[i].Path != w {
			t.Errorf("event %d path = %q, want %q", i, events[i].Path, w)
		}
	}
	if events[1].Err != "EACCES" || traceOp(events[1], map[string]bool{}) != "denied" {
		t.Errorf("a denied stat = %+v", events[1])
	}
}

func TestTraceNormalizeOffline(t *testing.T) {
	s := straceScope()
	for in, want := range map[string]string{
		"/sb/claude/projects/-sb-repo/0f8fad5b-d9cb-469f-a165-70867728950e.jsonl": "<claude-config>/projects/<repo-slug>/<uuid>.jsonl",
		"/tmp/claude-3f2a-cwd":           "<tmp>/claude-<id>-cwd",
		"/tmp/bun-node-a1b2c3d4e5f6a7b8": "<tmp>/bun-node-<hash>",
		"/sb/home/.config/gh/hosts.yml":  "<home>/.config/gh/hosts.yml",
		"/sbx/other":                     "/sbx/other",
		"/sb/claude/statsig/statsig.cached.evaluations.1234567890": "<claude-config>/statsig/statsig.cached.evaluations.<n>",
		// What real builds write: Claude Code's atomic writes and per-process files, Codex's
		// tempfile names, git's object folders, mkdtemp's and short hex names in /tmp.
		"/sb/claude/.claude.json.tmp.56.0e1234b5":                           "<claude-config>/.claude.json.tmp.<n>.<id>",
		"/sb/claude/.tmpG2j1eb":                                             "<claude-config>/.tmp<id>",
		"/sb/claude/sessions/56.json":                                       "<claude-config>/sessions/<n>.json",
		"/sb/repo/.git/objects/3a/abcdef0123456789abcdef0123456789abcdef01": "<repo>/.git/objects/<id>/<hash>",
		"/tmp/claude-1000/fswatch-probe-FgdbpF":                             "<tmp>/claude-<id>/fswatch-probe-<id>",
		"/tmp/claude-3f2e-cwd":                                              "<tmp>/claude-<id>-cwd",
		"/tmp/cc-socks/56.sock":                                             "<tmp>/cc-socks/<n>.sock",
		"/sb/home/.ssh/id_ed25519":                                          "<home>/.ssh/id_ed25519",
		"/sb/claude/sessions/2026/11/05/rollout-x.jsonl":                    "<claude-config>/sessions/<n>/<n>/<n>/rollout-x.jsonl",
		"/newroot/sb/repo/.git":                                             "/newroot/<repo>/.git",
		"/etc/os-release":                                                   "/etc/os-release",
		"/usr/share/zoneinfo/Etc/UTC":                                       "/usr/share/zoneinfo/Etc/UTC",
	} {
		if got := s.normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	// A command line: its paths, and the heredoc delimiters Claude Code makes for each run.
	for in, want := range map[string]string{
		`cat >> "$SNAPSHOT_FILE" << 'PATH_END_c3ph6ooejs'`:           `cat >> "$SNAPSHOT_FILE" << 'PATH_END_<id>'`,
		`cat >> "$SNAPSHOT_FILE" << 'CLAUDE_CODE_END_s77gn0jo7vj'`:   `cat >> "$SNAPSHOT_FILE" << 'CLAUDE_CODE_END_<id>'`,
		`cat >> "$SNAPSHOT_FILE" << 'RIPGREP_FUNC_END'`:              `cat >> "$SNAPSHOT_FILE" << 'RIPGREP_FUNC_END'`,
		`env CLAUDE_CODE_ENTRYPOINT=cli NODE_OPTIONS=--max_old=4096`: `env CLAUDE_CODE_ENTRYPOINT=cli NODE_OPTIONS=--max_old=4096`,
	} {
		if got := s.normalizeText(in); got != want {
			t.Errorf("normalizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// eslogger lines as Endpoint Security's JSON has them (the fields the trace reads, and enough
// of the rest to be realistic): the build (pid 500) execs, forks a shell (501), which
// posix_spawns git (502, known by its parent alone); an unrelated process (900) does the same
// things and is no part of the tree.
var esFixture = []string{
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:00.100000000Z","event_type":9,"process":{"audit_token":{"pid":500},"ppid":400,"original_ppid":400,"executable":{"path":"/Users/me/e2e.test"}},"event":{"exec":{"target":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"args":["claude","-p","Say hello."],"cwd":{"path":"/sb/repo"},"script":null}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:00.200000000Z","event_type":10,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"open":{"fflag":1,"file":{"path":"/sb/claude/.claude.json","path_truncated":false,"stat":{"st_size":17}}}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:00.300000000Z","event_type":43,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"lookup":{"source_dir":{"path":"/sb/repo"},"relative_target":"CLAUDE.md"}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:00.400000000Z","event_type":12,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"open":{"fflag":1,"file":{"path":"/sb/home/.aws/credentials"}}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:00.500000000Z","event_type":105,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"xpc_connect":{"service_name":"com.apple.securityd","service_domain_type":1}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.100000000Z","event_type":11,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"fork":{"child":{"audit_token":{"pid":501},"ppid":500,"executable":{"path":"/opt/claude/2.1.318/claude"}}}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.200000000Z","event_type":9,"process":{"audit_token":{"pid":501},"ppid":500,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"exec":{"target":{"audit_token":{"pid":501},"executable":{"path":"/bin/zsh"}},"args":["/bin/zsh","-c","git status"]}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.300000000Z","event_type":9,"process":{"audit_token":{"pid":502},"ppid":501,"executable":{"path":"/bin/zsh"}},"event":{"exec":{"target":{"audit_token":{"pid":502},"executable":{"path":"/usr/bin/git"}},"args":["git","status"]}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.400000000Z","event_type":10,"process":{"audit_token":{"pid":502},"ppid":501,"executable":{"path":"/usr/bin/git"}},"event":{"open":{"fflag":3,"file":{"path":"/sb/repo/.git/index.lock"}}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.500000000Z","event_type":25,"process":{"audit_token":{"pid":502},"ppid":501,"executable":{"path":"/usr/bin/git"}},"event":{"rename":{"source":{"path":"/sb/repo/.git/index.lock"},"destination_type":0,"destination":{"existing_file":{"path":"/sb/repo/.git/index"}}}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.600000000Z","event_type":13,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"create":{"destination_type":1,"destination":{"new_path":{"dir":{"path":"/sb/repo"},"filename":"notes.txt","mode":420}}}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.700000000Z","event_type":68,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"uipc_connect":{"file":{"path":"/private/tmp/td1/agent.sock"},"domain":1,"type":1,"protocol":0}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.800000000Z","event_type":87,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"get_task":{"target":{"audit_token":{"pid":77},"executable":{"path":"/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder"}},"type":1}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.900000000Z","event_type":87,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"get_task_read":{"target":{"audit_token":{"pid":502},"executable":{"path":"/usr/bin/git"}},"type":1}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.950000000Z","event_type":87,"process":{"audit_token":{"pid":502},"ppid":501,"executable":{"path":"/usr/bin/git"}},"event":{"get_task_read":{"target":{"audit_token":{"pid":78},"executable":{"path":"/Applications/Private.app/Contents/MacOS/Private"}},"type":1}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:03.100000000Z","event_type":119,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"btm_launch_item_add":{"instigator":null,"app":null,"item":{"item_type":2,"legacy":false,"managed":false,"uid":501,"item_url":"file:///sb/home/Library/LaunchAgents/com.example.agent.plist","app_url":null},"executable_path":"/sb/home/.local/bin/agentd"}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:03.200000000Z","event_type":32,"process":{"audit_token":{"pid":500},"ppid":400,"executable":{"path":"/opt/claude/2.1.318/claude"}},"event":{"unlink":{"target":{"path":"/sb/claude/session.lock"},"parent_dir":{"path":"/sb/claude"}}}}`,
	`{"schema_version":1,"version":7,"time":"2026-11-05T10:00:01.000000000Z","event_type":10,"process":{"audit_token":{"pid":900},"ppid":1,"executable":{"path":"/usr/libexec/mdworker"}},"event":{"open":{"fflag":1,"file":{"path":"/sb/home/.ssh/id_ed25519"}}}}`,
}

func TestFilesOfESLoggerOffline(t *testing.T) {
	var lines [][]byte
	for _, l := range esFixture {
		lines = append(lines, []byte(l))
	}
	events := ParseESLog(lines)
	scope := straceScope()
	scope.Platform, scope.Tracer, scope.Root = "darwin/arm64", "eslogger", 500
	scope.TurnStart = time.Date(2026, 11, 5, 10, 0, 1, 0, time.UTC)
	scope.TurnEnd = time.Date(2026, 11, 5, 10, 0, 3, 0, time.UTC)
	scope.Decoys = append(scope.Decoys, "/private/tmp/td1/agent.sock")
	scope.Replace["/private/tmp/td1/agent.sock"] = "<home>/.ssh/agent.sock"
	p := FilesOf(events, scope)
	for path, w := range map[string]PathUse{
		"<claude-config>/.claude.json": {Ops: []string{"read"}, Phases: []string{"startup"}},
		"<repo>/CLAUDE.md":             {Ops: []string{"probe"}, Phases: []string{"startup"}},
		"<repo>/.git/index.lock":       {Ops: []string{"write", "rename"}, Phases: []string{"turn"}, Programs: []string{"git"}},
		"<repo>/.git/index":            {Ops: []string{"write"}, Phases: []string{"turn"}, Programs: []string{"git"}},
		"<repo>/notes.txt":             {Ops: []string{"create"}, Phases: []string{"turn"}},
		"<claude-config>/session.lock": {Ops: []string{"delete"}, Phases: []string{"exit"}},
	} {
		got := p.Paths[path]
		if got == nil || !reflect.DeepEqual(*got, w) {
			t.Errorf("%s = %+v, want %+v (%v)", path, got, w, mapsKeys(p.Paths))
		}
	}
	if e := p.Exec["/bin/zsh -c git status"]; e == nil || e.By != "claude" {
		t.Errorf("zsh exec = %+v (%v)", e, mapsKeys(p.Exec))
	}
	if e := p.Exec["git status"]; e == nil || e.By != "zsh" {
		t.Errorf("git exec = %+v (%v)", e, mapsKeys(p.Exec))
	}
	// macOS has no read event: an open for reading is "opened". The other process's open of a
	// decoy is not the build's.
	for path, level := range map[string]string{"<home>/.aws/credentials": "opened", "<home>/.ssh/id_ed25519": "not touched",
		"<home>/.ssh/agent.sock": "connected"} {
		if p.Decoys[path] != level {
			t.Errorf("decoy %s = %q, want %q", path, p.Decoys[path], level)
		}
	}
	if p.MacOS == nil {
		t.Fatal("no macos part")
	}
	if p.MacOS.XPC["com.apple.securityd"] == nil {
		t.Errorf("xpc = %v", mapsKeys(p.MacOS.XPC))
	}
	if p.MacOS.LaunchItems["<home>/.local/bin/agentd"] == nil {
		t.Errorf("launch items = %v", mapsKeys(p.MacOS.LaunchItems))
	}
	// Another process's task port is recorded by its name; its own child's is not; a helper's, as
	// one entry, never what the machine runs.
	if got := mapsKeys(p.MacOS.TaskPorts); !reflect.DeepEqual(got, []string{"<processes>", "Finder"}) {
		t.Errorf("task ports = %v", got)
	}
}

func TestParseLsofOffline(t *testing.T) {
	raw := "p500\ncclaude\nf21\nPTCP\nn127.0.0.1:52345->127.0.0.1:43111\nTST=ESTABLISHED\nf22\nPTCP\nn127.0.0.1:53001\nTST=LISTEN\nf23\nPTCP\nn*:7777\nTST=LISTEN\np501\nczsh\nf5\nPUDP\nn*:5353\nf6\nPUDP\nn192.168.1.2:50000->8.8.8.8:53\n"
	at := time.Unix(10, 0)
	events := ParseLsof([]byte(raw), at)
	var got []string
	for _, ev := range events {
		got = append(got, strings.Join([]string{ev.Kind, ev.Sock.Proto, ev.Sock.Host, strconv.Itoa(ev.Sock.Port), boolWord(ev.Sock.Ephemeral)}, " "))
	}
	want := []string{"connect tcp 127.0.0.1 43111 fixed", "listen tcp 127.0.0.1 53001 ephemeral", "listen tcp  7777 fixed",
		"listen udp  5353 fixed", "connect udp 8.8.8.8 53 fixed"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lsof events:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func boolWord(eph bool) string {
	if eph {
		return "ephemeral"
	}
	return "fixed"
}

func TestSSHOptionsOffline(t *testing.T) {
	if got := sshOptions([]string{"ssh", "localhost"}); !reflect.DeepEqual(got, []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "localhost"}) {
		t.Errorf("sshOptions = %v", got)
	}
	if got := sshOptions([]string{"env", "X=1"}); !reflect.DeepEqual(got, []string{"env", "X=1"}) {
		t.Errorf("a prefix that is not ssh = %v", got)
	}
}

func TestThrowawaySSHKeyOffline(t *testing.T) {
	key := throwawaySSHKey(t)
	if !strings.HasPrefix(key, "-----BEGIN OPENSSH PRIVATE KEY-----\n") || throwawaySSHKey(t) == key {
		t.Errorf("key = %q", key)
	}
}

// macOS's /usr/bin/git is a stub that runs Xcode's git with the same arguments, in a child or
// in its own process: one program, started once. A git it starts with other arguments is its
// own.
func TestFilesOfStubHandOffOffline(t *testing.T) {
	xcode := "/Applications/Xcode.app/Contents/Developer/usr/bin/git"
	at := time.Unix(1000, 0)
	events := []TraceEvent{
		{Pid: 100, At: at, Kind: "exec", Path: "/opt/claude/2.1.318/claude", Argv: []string{"claude", "-p", "hi"}},
		{Pid: 100, At: at, Kind: "fork", Child: 101},
		{Pid: 101, At: at, Kind: "exec", Path: "/usr/bin/git", Argv: []string{"git", "status", "--short"}},
		{Pid: 101, At: at, Kind: "fork", Child: 102},
		{Pid: 102, At: at, Kind: "exec", Path: xcode, Argv: []string{xcode, "status", "--short"}},
		{Pid: 100, At: at, Kind: "fork", Child: 103},
		{Pid: 103, At: at, Kind: "exec", Path: "/usr/bin/git", Argv: []string{"git", "log", "-1"}},
		{Pid: 103, At: at, Kind: "exec", Path: xcode, Argv: []string{xcode, "log", "-1"}},
		{Pid: 103, At: at, Kind: "fork", Child: 104},
		{Pid: 104, At: at, Kind: "exec", Path: xcode, Argv: []string{xcode, "rev-parse", "HEAD"}},
	}
	scope := straceScope()
	scope.Root = 100
	got := slices.Sorted(maps.Keys(FilesOf(events, scope).Exec))
	want := []string{xcode + " rev-parse HEAD", "git log -1", "git status --short"}
	if !slices.Equal(got, want) {
		t.Errorf("exec = %q, want %q", got, want)
	}
}
