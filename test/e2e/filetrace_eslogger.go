package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Tracing on macOS: Endpoint Security, through eslogger, which logs every process's events on
// the machine as JSON lines. It must run as root and its responsible process needs Full Disk
// Access: the suite runs exactly `sudo -n /usr/bin/eslogger …` (a sudoers rule allows it and
// nothing else), directly or through TERMA_E2E_ESLOGGER_VIA, a command prefix that runs it on
// the same machine where the session has Full Disk Access (`ssh localhost`, Remote Login's
// "Allow full disk access for remote users"). eslogger has no process filter: the trace keeps
// the harness's process tree, followed from each event's parent and the forks it sees.
// eslogger leaves out the events of its own process group, so a direct one runs in a group of
// its own. It stops when its output pipe is closed (SIGPIPE at its next write; through SSH, the
// session's end). Endpoint Security has no TCP or UDP events: lsof samples the tree's sockets
// every 250 ms, which can miss one opened and closed between two samples.

// esEvents are the events traced, those this macOS has.
var esEvents = []string{
	"open", "close", "create", "rename", "unlink", "link", "stat", "lookup", "readdir", "access", "truncate", "write",
	"setextattr", "getextattr", "deleteextattr", "copyfile", "exec", "fork", "exit", "chdir", "uipc_bind", "uipc_connect",
	"xpc_connect", "btm_launch_item_add", "tcc_modify", "get_task", "get_task_read", "get_task_inspect",
}

// esLauncher is how eslogger is started: a prefix before `sudo -n /usr/bin/eslogger`, or none.
type esLauncher struct {
	prefix []string
	events []string
}

var (
	esOnce sync.Once
	esUse  *esLauncher
	esWhy  string
)

// esloggerAvailable is how this machine runs eslogger, or why it cannot: macOS, the sudoers rule,
// and Full Disk Access, directly or through TERMA_E2E_ESLOGGER_VIA.
func esloggerAvailable() (*esLauncher, string) {
	esOnce.Do(func() {
		if runtime.GOOS != "darwin" {
			esWhy = "eslogger is macOS's"
			return
		}
		var whys []string
		candidates := [][]string{nil}
		if via := strings.Fields(os.Getenv("TERMA_E2E_ESLOGGER_VIA")); len(via) > 0 {
			candidates = append(candidates, sshOptions(via))
		}
		for _, prefix := range candidates {
			l := &esLauncher{prefix: prefix}
			why := l.preflight()
			if why == "" {
				esUse = l
				return
			}
			name := "directly"
			if prefix != nil {
				name = "through " + strings.Join(prefix, " ")
			}
			whys = append(whys, name+": "+why)
		}
		esWhy = "eslogger cannot run " + strings.Join(whys, "; ")
	})
	return esUse, esWhy
}

// sshOptions is an ssh prefix with options that fail at once where there is no key, rather
// than ask for a password.
func sshOptions(via []string) []string {
	if filepath.Base(via[0]) != "ssh" {
		return via
	}
	out := []string{via[0]}
	for _, o := range []string{"BatchMode=yes", "ConnectTimeout=5"} {
		if !slices.Contains(via, o) && !slices.Contains(via, "-o"+o) {
			out = append(out, "-o", o)
		}
	}
	return append(out, via[1:]...)
}

// command is the eslogger command for events.
func (l *esLauncher) command(ctx context.Context, args ...string) *exec.Cmd {
	argv := append(append(slices.Clone(l.prefix), "sudo", "-n", "/usr/bin/eslogger"), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Its own process group: eslogger leaves out its group's events, which would be the
	// harness's too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The context ends it by closing its pipe, never by a signal to root's process.
	cmd.Cancel = func() error { return nil }
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

// preflight is why eslogger cannot trace this way, "" where it can: it lists its events, then
// logs an exec of the preflight's own.
func (l *esLauncher) preflight() string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := l.command(ctx, "--list-events").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("--list-events: %v: %s", err, strings.TrimSpace(string(out)))
	}
	have := strings.Fields(string(out))
	for _, e := range esEvents {
		if slices.Contains(have, e) {
			l.events = append(l.events, e)
		}
	}
	tr, err := l.start(ctx, []string{"exec"})
	if err != nil {
		return err.Error()
	}
	defer tr.stop()
	// An exec of the preflight's own, again until eslogger, once its client is up, logs one.
	tr.setRoot(-1)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = exec.Command("/usr/bin/true").Run()
		if tr.seen.Load() > 0 {
			return ""
		}
		if why := tr.failed(); why != "" {
			return why
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "no event arrived within 15s: " + tr.failed()
}

// esTrace is a running eslogger and the harness tree's lines it has kept.
type esTrace struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr bytes.Buffer
	done   chan struct{}
	ready  chan struct{}

	root atomic.Int64
	seen atomic.Int64 // lines read, the tree's or not
	mu   sync.Mutex
	// early are lines before the root was known; lines the tree's.
	early, lines [][]byte
	members      map[int]bool
	// lastSeq is the last message's number, lost how many Endpoint Security dropped.
	lastSeq, lost int64
	// rootExited is whether the harness's own exit has arrived: what eslogger and the pipe
	// still held of the run has, too.
	rootExited bool
	exited     bool
}

// start starts eslogger logging events.
func (l *esLauncher) start(ctx context.Context, events []string) (*esTrace, error) {
	cmd := l.command(ctx, append([]string{"--format", "json"}, events...)...)
	tr := &esTrace{cmd: cmd, done: make(chan struct{}), ready: make(chan struct{}), members: map[int]bool{}}
	cmd.Stderr = &lockedWriter{w: &tr.stderr, mu: &tr.mu}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tr.stdout = stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go tr.read()
	return tr, nil
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// read keeps the lines of the harness's tree as they arrive.
func (tr *esTrace) read() {
	defer close(tr.done)
	r := bufio.NewReaderSize(tr.stdout, 1<<20)
	first := true
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if first {
				close(tr.ready)
				first = false
			}
			tr.take(line)
		}
		if err != nil {
			break
		}
	}
	if first {
		close(tr.ready)
	}
	tr.mu.Lock()
	tr.exited = true
	tr.mu.Unlock()
}

// esHead is what a line says of its process, for following the tree.
type esHead struct {
	GlobalSeq int64     `json:"global_seq_num"`
	Process   esProcess `json:"process"`
	Event     struct {
		Fork *struct {
			Child esProcess `json:"child"`
		} `json:"fork"`
		Exit json.RawMessage `json:"exit"`
	} `json:"event"`
}

func (tr *esTrace) take(line []byte) {
	tr.seen.Add(1)
	root := int(tr.root.Load())
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if root == 0 {
		if len(tr.early) < 500000 {
			tr.early = append(tr.early, slices.Clone(line))
		}
		return
	}
	tr.keep(root, line)
}

// keep keeps line where its process is in the tree, adding the processes it starts.
func (tr *esTrace) keep(root int, line []byte) {
	var h esHead
	if json.Unmarshal(line, &h) != nil {
		return
	}
	// Endpoint Security numbers every message it sends a client: a gap is messages it dropped,
	// a client too slow for the machine's events.
	if tr.lastSeq > 0 && h.GlobalSeq > tr.lastSeq+1 {
		tr.lost += h.GlobalSeq - tr.lastSeq - 1
	}
	if h.GlobalSeq > tr.lastSeq {
		tr.lastSeq = h.GlobalSeq
	}
	pid := h.Process.AuditToken.Pid
	if !tr.members[pid] {
		if pid != root && !tr.members[h.Process.Ppid] && !tr.members[h.Process.OriginalPpid] {
			return
		}
		tr.members[pid] = true
	}
	if h.Event.Exit != nil && pid == root {
		tr.rootExited = true
	}
	if h.Event.Fork != nil {
		tr.members[h.Event.Fork.Child.AuditToken.Pid] = true
	}
	tr.lines = append(tr.lines, slices.Clone(line))
}

// setRoot names the harness's process: the lines before it are read again for it.
func (tr *esTrace) setRoot(pid int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.root.Store(int64(pid))
	tr.members[pid] = true
	for _, line := range tr.early {
		tr.keep(pid, line)
	}
	tr.early = nil
}

func (tr *esTrace) kept() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.lines)
}

// pids are the tree's processes so far.
func (tr *esTrace) pids() []int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]int, 0, len(tr.members))
	for pid := range tr.members {
		out = append(out, pid)
	}
	slices.Sort(out)
	return out
}

// failed is what eslogger said where it stopped, "" while it runs.
func (tr *esTrace) failed() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.exited {
		return ""
	}
	if msg := strings.TrimSpace(tr.stderr.String()); msg != "" {
		return msg
	}
	return "eslogger exited"
}

// stop ends eslogger by closing its output, and waits for it: it exits at its next write.
// It reports whether it is gone.
func (tr *esTrace) stop() error {
	_ = tr.stdout.Close()
	<-tr.done
	errc := make(chan error, 1)
	go func() { errc <- tr.cmd.Wait() }()
	select {
	case <-errc:
	case <-time.After(20 * time.Second):
		return errors.New("eslogger did not exit 20s after its output was closed")
	}
	return nil
}

// esloggerLeftovers are the eslogger processes still running, as pgrep lists them: eslogger,
// or the sudo or ssh that started it, by what its command starts with; a shell whose command
// line only names it is none.
func esloggerLeftovers() string {
	out, _ := exec.Command("pgrep", "-fl", "/usr/bin/eslogger").Output()
	var left []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		_, command, _ := strings.Cut(line, " ")
		if strings.HasPrefix(command, "/usr/bin/eslogger") || strings.HasPrefix(command, "sudo -n /usr/bin/eslogger") ||
			strings.HasPrefix(command, "ssh ") && strings.Contains(command, "/usr/bin/eslogger") {
			left = append(left, line)
		}
	}
	return strings.Join(left, "\n")
}

// runESLogger runs cmd traced by eslogger and sampled by lsof, and returns what it printed and
// the events of its tree; root is its process.
func runESLogger(t *testing.T, l *esLauncher, cmd *exec.Cmd, settle time.Duration) (out []byte, events []TraceEvent, root int, err error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr, err := l.start(ctx, l.events)
	if err != nil {
		return nil, nil, 0, err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = tr.stop()
		}
	}()
	select {
	case <-tr.ready:
	case <-time.After(30 * time.Second):
		return nil, nil, 0, errors.New("eslogger printed nothing in 30s")
	}
	if why := tr.failed(); why != "" {
		return nil, nil, 0, errors.New(why)
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return nil, nil, 0, err
	}
	root = cmd.Process.Pid
	tr.setRoot(root)
	sampled := make(chan []TraceEvent, 1)
	sampling, stopSampling := context.WithCancel(context.Background())
	go func() { sampled <- sampleSockets(sampling, tr.pids) }()
	err = cmd.Wait()
	// eslogger's output and the pipe (an SSH session's) hold events a while: the trace waits for
	// the build's own exit to come through before it settles. One that never does is short.
	short := true
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		tr.mu.Lock()
		exited := tr.rootExited
		tr.mu.Unlock()
		if exited {
			short = false
			break
		}
	}
	time.Sleep(settle)
	stopSampling()
	socks := <-sampled
	if serr := tr.stop(); serr != nil {
		t.Errorf("eslogger: %v", serr)
	}
	stopped = true
	if left := esloggerLeftovers(); left != "" {
		t.Logf("eslogger processes still running after the trace: %s", left)
	}
	tr.mu.Lock()
	lines, lost := tr.lines, tr.lost
	tr.mu.Unlock()
	if lost > 0 {
		t.Logf("Endpoint Security dropped %d of the machine's events while eslogger traced: the trace may lack the build's", lost)
	}
	if keep := os.Getenv("TERMA_E2E_TRACE_DIR"); keep != "" {
		_ = os.MkdirAll(keep, 0o755)
		name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + ".eslogger.jsonl"
		_ = os.WriteFile(filepath.Join(keep, name), bytes.Join(lines, nil), 0o644)
	}
	events = ParseESLog(lines)
	events = append(events, socks...)
	if short {
		t.Logf("eslogger never logged the build's own exit: the trace is short of the run's end")
		lost++
	}
	if lost > 0 {
		events = append(events, TraceEvent{Kind: "lost", Count: int(lost)})
	}
	slices.SortStableFunc(events, func(a, b TraceEvent) int { return a.At.Compare(b.At) })
	return buf.Bytes(), events, root, err
}

// sampleSockets lists the sockets of the tree's processes every 250 ms with lsof, until ctx
// ends: each listener and connection once, at the first sample that saw it.
func sampleSockets(ctx context.Context, pids func() []int) []TraceEvent {
	var out []TraceEvent
	seen := map[string]bool{}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		list := pids()
		if len(list) > 0 {
			args := []string{"-nP", "-a", "-i", "-F", "pcfPnT", "-p"}
			ids := make([]string, len(list))
			for i, p := range list {
				ids[i] = strconv.Itoa(p)
			}
			lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			raw, _ := exec.CommandContext(lctx, "/usr/sbin/lsof", append(args, strings.Join(ids, ","))...).Output()
			cancel()
			now := time.Now().UTC()
			for _, ev := range ParseLsof(raw, now) {
				key := fmt.Sprint(ev.Pid, ev.Kind, ev.Sock.Proto, ev.Sock.Host, ev.Sock.Port, ev.Name)
				if !seen[key] {
					seen[key] = true
					out = append(out, ev)
				}
			}
		}
		select {
		case <-ctx.Done():
			return out
		case <-tick.C:
		}
	}
}

// ParseLsof reads `lsof -F pcfPnT` into listen and connect events, at the sample's time. A
// macOS port from 49152 up is an ephemeral one.
func ParseLsof(raw []byte, at time.Time) []TraceEvent {
	var out []TraceEvent
	pid := 0
	var proto, name, fd string
	listen := false
	flush := func() {
		if name == "" || proto == "" {
			return
		}
		local, peer, connected := strings.Cut(name, "->")
		ev := TraceEvent{Pid: pid, At: at, Name: fd}
		switch {
		case connected:
			host, port := splitHostPort(peer)
			ev.Kind, ev.Sock = "connect", &TraceSocket{Proto: proto, Host: host, Port: port}
		case listen || proto == "udp":
			host, port := splitHostPort(local)
			if host == "*" {
				host = ""
			}
			ev.Kind, ev.Sock = "listen", &TraceSocket{Proto: proto, Host: host, Port: port, Ephemeral: port >= 49152}
		default:
			return
		}
		out = append(out, ev)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p':
			flush()
			pid, _ = strconv.Atoi(v)
			proto, name, fd, listen = "", "", "", false
		case 'f':
			flush()
			fd, proto, name, listen = v, "", "", false
		case 'P':
			proto = strings.ToLower(v)
		case 'n':
			name = v
		case 'T':
			if v == "ST=LISTEN" {
				listen = true
			}
		}
	}
	flush()
	return out
}

// esProcess is an Endpoint Security process, as eslogger prints one.
type esProcess struct {
	AuditToken struct {
		Pid int `json:"pid"`
	} `json:"audit_token"`
	Ppid         int    `json:"ppid"`
	OriginalPpid int    `json:"original_ppid"`
	Executable   esFile `json:"executable"`
}

type esFile struct {
	Path string `json:"path"`
}

// esDestination is a create's or rename's destination: a file that exists, or a new path.
type esDestination struct {
	ExistingFile *esFile `json:"existing_file"`
	NewPath      *struct {
		Dir      esFile `json:"dir"`
		Filename string `json:"filename"`
	} `json:"new_path"`
}

func (d *esDestination) path() string {
	switch {
	case d == nil:
		return ""
	case d.ExistingFile != nil:
		return d.ExistingFile.Path
	case d.NewPath != nil:
		return filepath.Join(d.NewPath.Dir.Path, d.NewPath.Filename)
	}
	return ""
}

// esLine is one eslogger event, the fields of every event the trace reads.
type esLine struct {
	Time    time.Time                  `json:"time"`
	Process esProcess                  `json:"process"`
	Event   map[string]json.RawMessage `json:"event"`
}

// esEvent holds the fields of the events the trace reads, by their eslogger names.
type esEvent struct {
	Target      json.RawMessage `json:"target"` // a file, or a process (get_task)
	File        *esFile         `json:"file"`
	Source      *esFile         `json:"source"`
	SourceDir   *esFile         `json:"source_dir"`
	RelTarget   string          `json:"relative_target"`
	Destination *esDestination  `json:"destination"`
	Dir         *esFile         `json:"dir"`
	Filename    string          `json:"filename"`
	TargetDir   *esFile         `json:"target_dir"`
	TargetName  string          `json:"target_name"`
	TargetFile  string          `json:"target_filename"`
	Fflag       int             `json:"fflag"`
	Modified    bool            `json:"modified"`
	Args        []string        `json:"args"`
	Child       *esProcess      `json:"child"`
	ServiceName string          `json:"service_name"`
	Service     string          `json:"service"`
	Identity    string          `json:"identity"`
	ExecPath    string          `json:"executable_path"`
	Item        *struct {
		ItemURL string `json:"item_url"`
	} `json:"item"`
}

// The open flags eslogger reports (fcntl.h's F-flags).
const (
	esFREAD  = 0x1
	esFWRITE = 0x2
	esOCREAT = 0x200
	esODIR   = 0x100000
)

// ParseESLog reads eslogger's JSON lines into events.
func ParseESLog(lines [][]byte) []TraceEvent {
	var out []TraceEvent
	for _, line := range lines {
		var l esLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		pid := l.Process.AuditToken.Pid
		ppid := l.Process.Ppid
		if ppid == 1 && l.Process.OriginalPpid != 0 {
			ppid = l.Process.OriginalPpid
		}
		for kind, raw := range l.Event {
			var e esEvent
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			ev := TraceEvent{Pid: pid, Ppid: ppid, At: l.Time.UTC()}
			target := func() string {
				var f esFile
				if json.Unmarshal(e.Target, &f) == nil {
					return f.Path
				}
				return ""
			}
			switch kind {
			case "exec":
				var p esProcess
				_ = json.Unmarshal(e.Target, &p)
				ev.Kind, ev.Path, ev.Argv = "exec", p.Executable.Path, e.Args
			case "fork":
				if e.Child == nil {
					continue
				}
				ev.Kind, ev.Child = "fork", e.Child.AuditToken.Pid
			case "open":
				if e.File == nil {
					continue
				}
				ev.Kind, ev.Path = "open", e.File.Path
				ev.Read, ev.Write, ev.Create = e.Fflag&esFREAD != 0, e.Fflag&esFWRITE != 0, e.Fflag&esOCREAT != 0
				if e.Fflag&esODIR != 0 && !ev.Write {
					ev.Kind = "list"
				}
			case "close":
				if !e.Modified {
					continue
				}
				ev.Kind, ev.Path = "write", target()
			case "create":
				ev.Kind, ev.Path = "create", e.Destination.path()
			case "rename":
				if e.Source == nil {
					continue
				}
				ev.Kind, ev.Path, ev.To = "rename", e.Source.Path, e.Destination.path()
			case "unlink":
				ev.Kind, ev.Path = "unlink", target()
			case "link":
				if e.TargetDir == nil {
					continue
				}
				ev.Kind, ev.Path = "link", filepath.Join(e.TargetDir.Path, e.TargetFile)
			case "stat", "access", "getextattr":
				ev.Kind, ev.Path = "stat", target()
			case "lookup":
				if e.SourceDir == nil {
					continue
				}
				ev.Kind, ev.Path = "lookup", filepath.Join(e.SourceDir.Path, e.RelTarget)
			case "readdir":
				ev.Kind, ev.Path = "list", target()
			case "truncate", "write", "setextattr", "deleteextattr":
				ev.Kind, ev.Path = "write", target()
			case "copyfile":
				if e.Source != nil {
					out = append(out, TraceEvent{Pid: pid, Ppid: ppid, At: ev.At, Kind: "open", Read: true, Path: e.Source.Path})
				}
				if e.TargetDir != nil {
					ev.Kind, ev.Path = "create", filepath.Join(e.TargetDir.Path, e.TargetName)
				}
			case "chdir":
				ev.Kind, ev.Path = "chdir", target()
			case "uipc_bind":
				if e.Dir == nil {
					continue
				}
				ev.Kind, ev.Sock = "bind", &TraceSocket{Proto: "unix", Host: filepath.Join(e.Dir.Path, e.Filename)}
			case "uipc_connect":
				if e.File == nil {
					continue
				}
				ev.Kind, ev.Sock = "connect", &TraceSocket{Proto: "unix", Host: e.File.Path}
			case "xpc_connect":
				ev.Kind, ev.Name = "xpc", e.ServiceName
			case "btm_launch_item_add":
				ev.Kind, ev.Name = "launch-item", e.ExecPath
				if ev.Name == "" && e.Item != nil {
					ev.Name = e.Item.ItemURL
				}
			case "tcc_modify":
				ev.Kind, ev.Name = "tcc", strings.TrimSpace(e.Service+" "+e.Identity)
			case "get_task", "get_task_read", "get_task_inspect":
				var p esProcess
				_ = json.Unmarshal(e.Target, &p)
				ev.Kind, ev.Name, ev.Child = "task-port", p.Executable.Path, p.AuditToken.Pid
			default:
				continue
			}
			if ev.Kind != "" {
				out = append(out, ev)
			}
		}
	}
	return out
}
