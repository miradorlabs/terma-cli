package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Tracing on Linux: the harness runs under strace, which follows every process and thread it
// starts (-f) and writes one line a system call to a file: the time (-ttt), the path of each
// descriptor and socket's addresses (-yy), strings up to 512 bytes (-s 512; file names are
// never cut). ParseStrace reads that file into TraceEvents. strace needs no root: it traces its
// own children. In a container, ptrace must be allowed (Docker's default seccomp profile allows
// it on current kernels; the machine job runs unconfined and with SYS_PTRACE).

// straceSyscalls are the calls traced: every call that names a file, every process and network
// call, and reads (to tell a decoy read from one only opened).
const straceSyscalls = "%file,%process,%network,read,pread64,readv,fchdir"

// straceArgs are strace's options for a trace written to out. -q, not -qq: -qq also drops the
// `+++ exited with N +++` lines, and exitWatch reads the traced command's status from them when
// strace is stopped while something it started still runs. -I2: strace running a command into a
// file ignores SIGTERM by default (-I3), and runStrace stops it with one.
func straceArgs(out string) []string {
	return []string{"-f", "-q", "-I2", "-ttt", "-yy", "-s", "512", "--pidns-translation", "-e", "signal=none", "-e", "trace=" + straceSyscalls, "-o", out}
}

var (
	straceOnce sync.Once
	straceWhy  string
)

// straceAvailable is why this machine cannot trace with strace, "" where it can: Linux, strace
// installed, and ptrace allowed.
func straceAvailable() string {
	straceOnce.Do(func() {
		path, err := exec.LookPath("strace")
		if err != nil {
			straceWhy = "strace is not installed"
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, "-f", "-qq", "-o", "/dev/null", "true").CombinedOutput()
		if err != nil {
			straceWhy = fmt.Sprintf("strace cannot trace here (ptrace not allowed?): %v: %s", err, strings.TrimSpace(string(out)))
		}
	})
	return straceWhy
}

// runStrace runs cmd under strace and returns what it printed and the events of its trace;
// root is the traced command's process. strace -f waits for every process it follows, and a
// hook leaves terma's relay running: settle after the command's own process exits, strace is
// stopped (it detaches what still runs, and writes out what it holds), and the command's exit
// status is read from the trace. TERMA_E2E_TRACE_DIR keeps each raw trace there.
func runStrace(t *testing.T, cmd *exec.Cmd, settle time.Duration) (out []byte, events []TraceEvent, root int, err error) {
	t.Helper()
	strace, lerr := exec.LookPath("strace")
	if lerr != nil {
		return nil, nil, 0, lerr
	}
	dir := t.TempDir()
	if keep := os.Getenv("TERMA_E2E_TRACE_DIR"); keep != "" {
		dir, _ = filepath.Abs(keep) // the traced command runs in its repository
		_ = os.MkdirAll(dir, 0o755)
	}
	file := filepath.Join(dir, strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())+".strace")
	cmd.Args = append(append(append([]string{strace}, straceArgs(file)...), "--", cmd.Path), cmd.Args[1:]...)
	cmd.Path = strace
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return nil, nil, 0, err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var werr error
	child := 0
	ended := false
wait:
	for {
		select {
		case werr = <-waited:
			// strace ended by itself: everything it followed exited.
			ended = true
			break wait
		case <-time.After(200 * time.Millisecond):
			if child == 0 {
				child = firstChild(cmd.Process.Pid)
			} else if !running(child) {
				break wait
			}
		}
	}
	if !ended {
		select {
		case werr = <-waited:
		case <-time.After(settle):
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case werr = <-waited:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				werr = <-waited
			}
		}
	}
	watch := &exitWatch{file: file}
	if status, serr := watch.status(); status >= 0 {
		err = serr
	} else if ended {
		err = werr
	}
	f, ferr := os.Open(file)
	if ferr != nil {
		return buf.Bytes(), nil, 0, ferr
	}
	defer f.Close()
	events, root = ParseStrace(f)
	return buf.Bytes(), events, root, err
}

// firstChild is the first process pid started, 0 while it has none.
func firstChild(pid int) int {
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	for _, task := range tasks {
		data, _ := os.ReadFile(task)
		if f := strings.Fields(string(data)); len(f) > 0 {
			n, _ := strconv.Atoi(f[0])
			return n
		}
	}
	return 0
}

// running reports a process that has not exited: there, and no zombie.
func running(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// pid (comm) state …: the state follows the command's closing parenthesis.
	i := strings.LastIndexByte(string(data), ')')
	return i < 0 || i+2 >= len(data) || data[i+2] != 'Z' && data[i+2] != 'X'
}

// rootExit matches the first process's end in a trace.
var rootExit = regexp.MustCompile(`(?m)^(\d+)\s+\d+\.\d+\s+\+\+\+ (?:exited with (\d+)|killed by (\w+))`)

// exitWatch reads a growing trace for its first process's end.
type exitWatch struct {
	file  string
	off   int64
	first string
	tail  string
}

// status is how the trace's first process ended, -1 while it runs; an error where it failed.
func (w *exitWatch) status() (int, error) {
	f, err := os.Open(w.file)
	if err != nil {
		return -1, nil
	}
	defer f.Close()
	if _, err := f.Seek(w.off, io.SeekStart); err != nil {
		return -1, nil
	}
	data, _ := io.ReadAll(f)
	w.off += int64(len(data))
	text := w.tail + string(data)
	cut := strings.LastIndexByte(text, '\n')
	if cut < 0 {
		w.tail = text
		return -1, nil
	}
	text, w.tail = text[:cut+1], text[cut+1:]
	if w.first == "" {
		w.first, _, _ = strings.Cut(text, " ")
	}
	for _, m := range rootExit.FindAllStringSubmatch(text, -1) {
		if m[1] != strings.TrimSpace(w.first) {
			continue
		}
		if m[3] != "" {
			return 1, fmt.Errorf("killed by %s", m[3])
		}
		code, _ := strconv.Atoi(m[2])
		if code != 0 {
			return code, fmt.Errorf("exit status %d", code)
		}
		return 0, nil
	}
	return -1, nil
}

// straceLine is a line of strace -f -ttt: the process, the time, and the call.
var straceLine = regexp.MustCompile(`^(\d+)\s+(\d+\.\d+)\s+(.*)$`)

// ParseStrace reads a trace strace -f -ttt -yy wrote into events, and the first process's id
// (the traced command's). A call another process interrupted (<unfinished ...>) is read whole
// where it resumes, at the time it started.
func ParseStrace(r io.Reader) ([]TraceEvent, int) {
	type pending struct {
		at   time.Time
		text string
	}
	var (
		events []TraceEvent
		root   int
	)
	waiting := map[int]pending{}
	p := &straceParser{cwd: map[int]string{}, ephemeral: map[string]bool{}, fds: map[int]map[int]string{}, tgid: map[int]int{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		m := straceLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		if root == 0 {
			root = pid
		}
		at := epochTime(m[2])
		rest := m[3]
		switch {
		case strings.HasPrefix(rest, "+++") || strings.HasPrefix(rest, "---"):
			continue
		case strings.HasSuffix(rest, "<unfinished ...>"):
			waiting[pid] = pending{at, strings.TrimSuffix(strings.TrimSuffix(rest, "<unfinished ...>"), " ")}
			continue
		case strings.HasPrefix(rest, "<... "):
			i := strings.Index(rest, " resumed>")
			w, ok := waiting[pid]
			if i < 0 || !ok {
				continue
			}
			delete(waiting, pid)
			rest, at = w.text+strings.TrimPrefix(rest[i+len(" resumed>"):], " "), w.at
		}
		events = append(events, p.call(pid, at, rest)...)
	}
	return events, root
}

// epochTime is strace's -ttt seconds.micros as a time.
func epochTime(s string) time.Time {
	sec, frac, _ := strings.Cut(s, ".")
	secs, _ := strconv.ParseInt(sec, 10, 64)
	frac = (frac + "000000000")[:9]
	nanos, _ := strconv.ParseInt(frac, 10, 64)
	return time.Unix(secs, nanos).UTC()
}

// straceParser keeps what a trace says across lines: each process's working directory, and
// which sockets were bound to any port.
type straceParser struct {
	cwd       map[int]string
	ephemeral map[string]bool // pid/fd
	// fds are the paths of the descriptors each process's calls named, by its thread group (a
	// thread's process): a path through /proc/self/fd/N names N's. A forked process starts
	// with its parent's.
	fds  map[int]map[int]string
	tgid map[int]int
}

// group is the process a thread belongs to.
func (p *straceParser) group(pid int) int {
	if g, ok := p.tgid[pid]; ok {
		return g
	}
	return pid
}

// fdArg is a descriptor -yy names a file's path: "15</repo>".
var fdArg = regexp.MustCompile(`(?:^|[(, ])(\d+)</([^<>"]*)>`)

// noteFDs keeps the paths of the descriptors a call names.
func (p *straceParser) noteFDs(pid int, text string) {
	for _, m := range fdArg.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		g := p.group(pid)
		if p.fds[g] == nil {
			p.fds[g] = map[int]string{}
		}
		p.fds[g][n] = "/" + m[2]
	}
}

// procFD is a path through a descriptor: /proc/self/fd/15/notes.txt.
var procFD = regexp.MustCompile(`^/(?:proc/(?:self|\d+)|dev)/fd/(\d+)(/.*)?$`)

// pidnsRE is the process id --pidns-translation adds of a child in another PID namespace.
var pidnsRE = regexp.MustCompile(`/\* (\d+) in strace's PID NS \*/`)

// throughFD is path with a descriptor's folder in place of /proc/self/fd/N, where the trace
// named N's path.
func (p *straceParser) throughFD(pid int, path string) string {
	m := procFD.FindStringSubmatch(path)
	if m == nil || m[2] == "" {
		return path
	}
	n, _ := strconv.Atoi(m[1])
	dir := p.fds[p.group(pid)][n]
	if dir == "" {
		return path
	}
	return filepath.Join(dir, m[2])
}

// straceCall is one system call: its name, arguments as printed, and what it returned.
type straceCall struct {
	name string
	args []string
	ret  string
	err  string
}

func parseStraceCall(text string) (straceCall, bool) {
	open := strings.IndexByte(text, '(')
	if open <= 0 {
		return straceCall{}, false
	}
	c := straceCall{name: text[:open]}
	args, end := splitArgs(text[open+1:])
	if end < 0 {
		return straceCall{}, false
	}
	c.args = args
	ret := strings.TrimSpace(text[open+1+end+1:])
	ret = strings.TrimSpace(strings.TrimPrefix(ret, "="))
	c.ret = ret
	if strings.HasPrefix(ret, "-1 ") {
		f := strings.Fields(ret)
		if len(f) > 1 {
			c.err = f[1]
		}
	}
	return c, true
}

// splitArgs splits a call's arguments at its top-level commas, to its closing parenthesis,
// and returns where that is (-1 where there is none). Strings, structures, lists and the
// paths -yy prints in angle brackets are each one argument.
func splitArgs(s string) ([]string, int) {
	var args []string
	depth, start := 0, 0
	inStr := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch ch {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '(', '[', '{':
			depth++
		case '<':
			depth++
		case '>':
			if i > 0 && (s[i-1] == '=' || s[i-1] == '-') {
				continue // "[28 => 16]", "a->b"
			}
			depth--
		case ']', '}':
			depth--
		case ')':
			if depth == 0 {
				if a := strings.TrimSpace(s[start:i]); a != "" || len(args) > 0 {
					args = append(args, a)
				}
				return args, i
			}
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return args, -1
}

// truncated is a string strace cut at its -s length, ended at its last whole line (or word)
// and marked: where the cut falls moves with the run's own values (a path's length) before it.
func truncated(v string) string {
	if i := strings.LastIndexByte(v, '\n'); i > 0 {
		return v[:i] + "\n…"
	}
	if i := strings.LastIndexByte(v, ' '); i > 0 {
		return v[:i] + " …"
	}
	return v + "…"
}

// unquote is a string argument's value: "" where it is no string.
func unquote(arg string) (string, bool) {
	arg, cut := strings.CutSuffix(arg, "...")
	if !strings.HasPrefix(arg, `"`) && !strings.HasPrefix(arg, `@"`) {
		return "", false
	}
	abstract := strings.HasPrefix(arg, "@")
	arg = strings.TrimPrefix(arg, "@")
	if len(arg) < 2 || arg[len(arg)-1] != '"' {
		return "", false
	}
	v := string(cUnescape(arg[1 : len(arg)-1]))
	if cut {
		v = truncated(v)
	}
	if abstract {
		v = "@" + v
	}
	return v, true
}

// cUnescape decodes strace's escapes: \n \t \" \\, \xNN and octal \NNN.
func cUnescape(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out = append(out, s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case 'v':
			out = append(out, '\v')
		case 'f':
			out = append(out, '\f')
		case 'x':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					out = append(out, byte(v))
					i += 2
					continue
				}
			}
			out = append(out, 'x')
		default:
			if c >= '0' && c <= '7' {
				j := i
				for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
					j++
				}
				v, _ := strconv.ParseUint(s[i:j], 8, 16)
				out = append(out, byte(v))
				i = j - 1
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// annotation is what -yy printed of a descriptor in angle brackets ("3</etc/hosts>",
// "AT_FDCWD</repo>", "5<TCP:[127.0.0.1:80]>"), without a device's own annotation.
func annotation(arg string) string {
	i := strings.IndexByte(arg, '<')
	if i < 0 || !strings.HasSuffix(arg, ">") {
		return ""
	}
	v := arg[i+1 : len(arg)-1]
	if j := strings.Index(v, "<char "); j >= 0 {
		v = v[:j]
	}
	if j := strings.Index(v, "<block "); j >= 0 {
		v = v[:j]
	}
	return v
}

// fdNumber is a descriptor argument's number, -1 for AT_FDCWD or none.
func fdNumber(arg string) int {
	n, err := strconv.Atoi(strings.SplitN(arg, "<", 2)[0])
	if err != nil {
		return -1
	}
	return n
}

// resolve is a path argument made absolute against its directory descriptor (or the
// process's working directory), "" where the call names a descriptor alone.
func (p *straceParser) resolve(pid int, dirArg, pathArg string) string {
	path, ok := unquote(pathArg)
	if !ok || path == "" {
		return ""
	}
	if dirArg != "" {
		if dir := annotation(dirArg); strings.HasPrefix(dirArg, "AT_FDCWD") && dir != "" {
			p.cwd[pid] = dir
		}
	}
	if filepath.IsAbs(path) {
		return p.throughFD(pid, filepath.Clean(path))
	}
	base := ""
	if dirArg != "" {
		base = annotation(dirArg)
	}
	if base == "" {
		base = p.cwd[pid]
	}
	if base == "" || !filepath.IsAbs(base) {
		return path
	}
	return filepath.Join(base, path)
}

// pathCalls are the file calls by where their directory descriptor and path are: -1 for no
// directory, and the kind of event each is.
var pathCalls = map[string]struct {
	dir, path int
	kind      string
}{
	"stat": {-1, 0, "stat"}, "lstat": {-1, 0, "stat"}, "newfstatat": {0, 1, "stat"}, "fstatat64": {0, 1, "stat"},
	"statx": {0, 1, "stat"}, "access": {-1, 0, "stat"}, "faccessat": {0, 1, "stat"}, "faccessat2": {0, 1, "stat"},
	"readlink": {-1, 0, "stat"}, "readlinkat": {0, 1, "stat"}, "statfs": {-1, 0, "stat"}, "getxattr": {-1, 0, "stat"},
	"lgetxattr": {-1, 0, "stat"}, "listxattr": {-1, 0, "stat"}, "llistxattr": {-1, 0, "stat"},
	"utimensat": {0, 1, "write"}, "utime": {-1, 0, "write"}, "utimes": {-1, 0, "write"}, "chmod": {-1, 0, "write"},
	"fchmodat": {0, 1, "write"}, "chown": {-1, 0, "write"}, "lchown": {-1, 0, "write"}, "fchownat": {0, 1, "write"},
	"truncate": {-1, 0, "write"}, "setxattr": {-1, 0, "write"}, "lsetxattr": {-1, 0, "write"},
	"mkdir": {-1, 0, "mkdir"}, "mkdirat": {0, 1, "mkdir"}, "rmdir": {-1, 0, "rmdir"}, "unlink": {-1, 0, "unlink"},
	"mknod": {-1, 0, "mkdir"}, "mknodat": {0, 1, "mkdir"}, "inotify_add_watch": {-1, 1, "watch"},
}

// call is the events one call makes.
func (p *straceParser) call(pid int, at time.Time, text string) []TraceEvent {
	c, ok := parseStraceCall(text)
	if !ok {
		return nil
	}
	p.noteFDs(pid, text)
	ev := TraceEvent{Pid: pid, At: at, Err: c.err}
	arg := func(i int) string {
		if i >= 0 && i < len(c.args) {
			return c.args[i]
		}
		return ""
	}
	switch c.name {
	case "open", "openat", "openat2", "creat":
		flags := arg(1)
		ev.Path = p.resolve(pid, "", arg(0))
		if c.name == "openat" || c.name == "openat2" {
			flags = arg(2)
			ev.Path = p.resolve(pid, arg(0), arg(1))
		}
		if c.name == "creat" {
			flags = "O_WRONLY|O_CREAT|O_TRUNC"
		}
		ev.Kind = "open"
		ev.Write = strings.Contains(flags, "O_WRONLY") || strings.Contains(flags, "O_RDWR")
		ev.Read = !strings.Contains(flags, "O_WRONLY")
		ev.Create = strings.Contains(flags, "O_CREAT")
		switch {
		case strings.Contains(flags, "O_PATH"):
			ev.Kind = "stat"
		case strings.Contains(flags, "O_DIRECTORY") && !ev.Write:
			ev.Kind = "list"
		}
	case "execve", "execveat":
		ev.Kind = "exec"
		ev.Path = p.resolve(pid, "", arg(0))
		ev.Argv = parseList(arg(1))
		if c.name == "execveat" {
			ev.Path = p.resolve(pid, arg(0), arg(1))
			ev.Argv = parseList(arg(2))
		}
	case "rename", "renameat", "renameat2", "link", "linkat", "symlink", "symlinkat":
		ev.Kind = "rename"
		switch c.name {
		case "rename":
			ev.Path, ev.To = p.resolve(pid, "", arg(0)), p.resolve(pid, "", arg(1))
		case "renameat", "renameat2":
			ev.Path, ev.To = p.resolve(pid, arg(0), arg(1)), p.resolve(pid, arg(2), arg(3))
		case "link":
			ev.Kind, ev.Path = "link", p.resolve(pid, "", arg(1))
		case "linkat":
			ev.Kind, ev.Path = "link", p.resolve(pid, arg(2), arg(3))
		case "symlink":
			ev.Kind, ev.Path = "link", p.resolve(pid, "", arg(1))
		case "symlinkat":
			ev.Kind, ev.Path = "link", p.resolve(pid, arg(1), arg(2))
		}
	case "unlinkat":
		ev.Kind = "unlink"
		if strings.Contains(arg(2), "AT_REMOVEDIR") {
			ev.Kind = "rmdir"
		}
		ev.Path = p.resolve(pid, arg(0), arg(1))
	case "chdir":
		ev.Kind = "chdir"
		ev.Path = p.resolve(pid, "", arg(0))
		if c.err == "" && ev.Path != "" {
			p.cwd[pid] = ev.Path
		}
	case "fchdir":
		if dir := annotation(arg(0)); c.err == "" && dir != "" {
			p.cwd[pid] = dir
		}
		return nil
	case "read", "pread64", "readv":
		path := annotation(arg(0))
		if !strings.HasPrefix(path, "/") || c.err != "" || c.ret == "0" {
			return nil
		}
		ev.Kind, ev.Path = "read", path
	case "clone", "clone3", "fork", "vfork":
		// A child in a new PID namespace (a sandbox's) has its own number there: strace says ours.
		ret := c.ret
		if m := pidnsRE.FindStringSubmatch(ret); m != nil {
			ret = m[1]
		}
		child, err := strconv.Atoi(strings.Fields(ret + " ")[0])
		if err != nil || child <= 0 {
			return nil
		}
		ev.Kind, ev.Child = "fork", child
		ev.Thread = strings.Contains(text, "CLONE_THREAD")
		if ev.Thread {
			p.tgid[child] = p.group(pid)
		} else if parent := p.fds[p.group(pid)]; parent != nil {
			p.fds[child] = maps.Clone(parent)
		}
		if cwd := p.cwd[pid]; cwd != "" {
			p.cwd[child] = cwd
		}
	case "connect", "bind":
		sock := p.sockaddr(arg(0), arg(1))
		if sock == nil {
			return nil
		}
		ev.Kind, ev.Sock = c.name, sock
		if c.name == "bind" && sock.Port == 0 && sock.Proto != "unix" {
			p.ephemeral[strconv.Itoa(pid)+"/"+strconv.Itoa(fdNumber(arg(0)))] = true
			sock.Ephemeral = true
		}
	case "listen":
		proto, local, _ := socketAnnotation(annotation(arg(0)))
		host, port := splitHostPort(local)
		if proto == "" || proto == "unix" {
			return nil
		}
		ev.Kind = "listen"
		ev.Sock = &TraceSocket{Proto: proto, Host: host, Port: port,
			Ephemeral: p.ephemeral[strconv.Itoa(pid)+"/"+strconv.Itoa(fdNumber(arg(0)))] || port >= 32768}
	case "sendto", "sendmsg", "sendmmsg":
		// A query to a resolver: the name it asks for.
		_, _, peer := socketAnnotation(annotation(arg(0)))
		_, port := splitHostPort(peer)
		if sock := p.sockaddr(arg(0), arg(4)); sock != nil && c.name == "sendto" {
			port = sock.Port
		}
		if port != 53 {
			return nil
		}
		var out []TraceEvent
		for _, q := range dnsQueries(text) {
			out = append(out, TraceEvent{Pid: pid, At: at, Kind: "dns", Name: q})
		}
		return out
	default:
		pc, known := pathCalls[c.name]
		if !known {
			return nil
		}
		dir := ""
		if pc.dir >= 0 {
			dir = arg(pc.dir)
		}
		ev.Kind = pc.kind
		ev.Path = p.resolve(pid, dir, arg(pc.path))
		if ev.Kind == "watch" {
			ev.Kind = "stat"
		}
	}
	if ev.Kind == "" || ev.Path == "" && ev.Sock == nil && ev.Kind != "fork" {
		return nil
	}
	return []TraceEvent{ev}
}

// parseList is a list of strings as strace prints one: ["git", "status"].
func parseList(arg string) []string {
	arg = strings.TrimSpace(arg)
	if !strings.HasPrefix(arg, "[") || !strings.HasSuffix(arg, "]") {
		return nil
	}
	parts, _ := splitArgs(arg[1:len(arg)-1] + ")")
	var out []string
	for _, part := range parts {
		if v, ok := unquote(part); ok {
			out = append(out, v)
		}
	}
	return out
}

var (
	saFamily = regexp.MustCompile(`sa_family=(AF_\w+)`)
	saPort   = regexp.MustCompile(`sin6?_port=htons\((\d+)\)`)
	saAddr4  = regexp.MustCompile(`sin_addr=inet_addr\("([^"]+)"\)`)
	saAddr6  = regexp.MustCompile(`inet_pton\(AF_INET6, "([^"]+)"`)
	saPath   = regexp.MustCompile(`sun_path=(@?"(?:[^"\\]|\\.)*")`)
	iovBase  = regexp.MustCompile(`iov_base="((?:[^"\\]|\\.)*)"`)
	sendData = regexp.MustCompile(`^sendto\([^,]+, "((?:[^"\\]|\\.)*)"`)
)

// sockaddr is the socket a call's descriptor and address argument name.
func (p *straceParser) sockaddr(fdArg, addrArg string) *TraceSocket {
	fam := saFamily.FindStringSubmatch(addrArg)
	if fam == nil {
		return nil
	}
	proto, _, _ := socketAnnotation(annotation(fdArg))
	switch fam[1] {
	case "AF_UNIX":
		m := saPath.FindStringSubmatch(addrArg)
		if m == nil {
			return nil
		}
		path, _ := unquote(m[1])
		return &TraceSocket{Proto: "unix", Host: path}
	case "AF_INET", "AF_INET6":
		s := &TraceSocket{Proto: proto}
		if s.Proto == "" || s.Proto == "unix" {
			s.Proto = "tcp"
		}
		if m := saPort.FindStringSubmatch(addrArg); m != nil {
			s.Port, _ = strconv.Atoi(m[1])
		}
		if m := saAddr4.FindStringSubmatch(addrArg); m != nil {
			s.Host = m[1]
		} else if m := saAddr6.FindStringSubmatch(addrArg); m != nil {
			s.Host = m[1]
		}
		return s
	}
	return nil
}

// socketAnnotation reads -yy's socket annotation: "TCP:[127.0.0.1:5->127.0.0.1:80]" is tcp,
// its local and its peer address.
func socketAnnotation(a string) (proto, local, peer string) {
	kind, rest, ok := strings.Cut(a, ":[")
	if !ok {
		return "", "", ""
	}
	switch {
	case strings.HasPrefix(kind, "TCP"):
		proto = "tcp"
	case strings.HasPrefix(kind, "UDP"):
		proto = "udp"
	case strings.HasPrefix(kind, "UNIX"):
		proto = "unix"
	default:
		return "", "", ""
	}
	rest = strings.TrimSuffix(rest, "]")
	local, peer, _ = strings.Cut(rest, "->")
	return proto, local, peer
}

// splitHostPort is an address's host and port: 0 where it has none (an inode alone).
func splitHostPort(addr string) (string, int) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", 0
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0
	}
	return strings.Trim(addr[:i], "[]"), port
}

// dnsQueries are the names the DNS queries a send call carries ask for.
func dnsQueries(text string) []string {
	var datas []string
	if m := sendData.FindStringSubmatch(text); m != nil {
		datas = append(datas, m[1])
	}
	for _, m := range iovBase.FindAllStringSubmatch(text, -1) {
		datas = append(datas, m[1])
	}
	var out []string
	for _, d := range datas {
		if name := dnsName(cUnescape(d)); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// dnsName is the name a DNS query message asks for, "" where it is none.
func dnsName(msg []byte) string {
	if len(msg) < 13 || msg[2]&0x80 != 0 { // a response, not a query
		return ""
	}
	var labels []string
	for i := 12; i < len(msg); {
		n := int(msg[i])
		if n == 0 {
			return strings.Join(labels, ".")
		}
		if n > 63 || i+1+n > len(msg) {
			return ""
		}
		labels = append(labels, string(msg[i+1:i+1+n]))
		i += 1 + n
	}
	return ""
}
