package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// T3 Code (pingdotgg/t3code, `t3` on npm) is a local server with a web front end that
// runs other agents: `codex app-server` (one per T3 thread, in the thread's workspace)
// and Claude Code through the Agent SDK (setting sources user, project and local),
// with T3's own environment — it sets no OTEL_* of its own (0.0.42). So the agents'
// user-level exporters and the repository's hooks work as they do anywhere, and the
// relay needs nothing T3-specific; these scenarios prove it. T3 is driven the way its
// front end drives it: commands POSTed to /api/orchestration/dispatch with a session
// token, state read from /api/orchestration/snapshot.

func forEachT3(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
	}
	path, err := exec_LookPath("t3")
	if err != nil {
		Record(t.Name(), "not run", "t3 is not installed")
		t.Skip("t3 is not installed")
	}
	b := Binary{Harness: "t3", Version: t3Version(path), Path: path, Installed: true}
	t.Run(b.Label(), func(t *testing.T) { run(t, b) })
}

// t3Version reads the npm package's version: `t3 <anything> --help` starts the server.
func t3Version(launcher string) string {
	real, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(real), "..", "package.json"))
	if err != nil {
		return "unknown"
	}
	var pkg struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(data, &pkg)
	return pkg.Version
}

// T3 is a running `t3 serve` and a client for it.
type T3 struct {
	t     *testing.T
	sb    *Sandbox
	url   string
	token string
	env   []string
	bin   string
	base  string
	cmd   *exec.Cmd
	out   bytes.Buffer
	once  sync.Once
}

// StartT3 serves T3 for the sandbox, with its state under the sandbox and env added to
// what its agents inherit (a fake model's address, say).
func (sb *Sandbox) StartT3(b Binary, env ...string) *T3 {
	t := sb.T
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	x := &T3{t: t, sb: sb, url: fmt.Sprintf("http://127.0.0.1:%d", port), bin: b.Path, base: filepath.Join(sb.Dir, "t3")}
	// T3 prepends the PATH of `$SHELL -ilc` to its own: a plain sh keeps the sandbox's.
	x.env = append(append(sb.termaEnv(), "SHELL=/bin/sh", "T3CODE_HOME="+x.base, "T3CODE_TELEMETRY_ENABLED=false"), env...)
	x.cmd = exec.Command(b.Path, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--base-dir", x.base, sb.Repo)
	x.cmd.Dir = sb.Repo
	x.cmd.Env = x.env
	x.cmd.Stdout, x.cmd.Stderr = &x.out, &x.out
	// `t3` is a Node launcher around the native server: stop them as one group, or the
	// server outlives the launcher holding its output pipes open.
	x.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	x.cmd.WaitDelay = 5 * time.Second
	if err := x.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.Stop)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := t3Client.Get(x.url + "/"); err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	issue := exec.CommandContext(ctx, b.Path, "auth", "session", "issue", "--token-only", "--base-dir", x.base)
	issue.Env = x.env
	// The launcher's native child can hold the output pipe past the launcher's exit.
	issue.WaitDelay = 5 * time.Second
	tok, err := issue.Output()
	if err != nil {
		t.Fatalf("t3 auth session issue: %v\n%s", err, x.out.String())
	}
	x.token = strings.TrimSpace(string(tok))
	return x
}

// t3Client bounds every request: a server that accepts and never answers must fail the
// scenario, not hang it past the test's own deadline.
var t3Client = &http.Client{Timeout: 30 * time.Second}

// Stop ends the server and the agents it started: the launcher's whole process group.
func (x *T3) Stop() {
	x.once.Do(func() {
		if x.cmd.Process == nil {
			return
		}
		pgid := x.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGINT)
		done := make(chan struct{})
		go func() { _ = x.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		_ = syscall.Kill(-pgid, syscall.SIGKILL) // anything of the group left behind
	})
}

func (x *T3) request(method, path string, body any) []byte {
	x.t.Helper()
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, x.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+x.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t3Client.Do(req)
	if err != nil {
		x.t.Fatalf("t3 %s %s: %v\n%s", method, path, err, tail(x.out.String(), 2000))
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		x.t.Fatalf("t3 %s %s: %s\n%s", method, path, resp.Status, data)
	}
	return data
}

func (x *T3) dispatch(cmd map[string]any) {
	x.t.Helper()
	cmd["commandId"] = uuid.NewString()
	cmd["createdAt"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	x.request(http.MethodPost, "/api/orchestration/dispatch", cmd)
}

// Project registers a workspace with T3 and returns its id.
func (x *T3) Project(dir string) string {
	x.t.Helper()
	id := "proj-" + uuid.NewString()[:8]
	x.dispatch(map[string]any{"type": "project.create", "projectId": id, "title": filepath.Base(dir), "workspaceRoot": dir})
	return id
}

// T3 agents: Codex through its app-server, Claude Code through the Agent SDK.
var (
	T3Codex  = map[string]any{"instanceId": "codex", "model": "gpt-5.1-codex"}
	T3Claude = map[string]any{"instanceId": "claudeAgent", "model": "claude-haiku-4-5"}
)

// Turn runs one turn of a new thread in project with agent and waits until T3 reports
// the thread's latest turn settled; anything but "completed" fails the scenario.
func (x *T3) Turn(project string, agent map[string]any, prompt string) {
	x.t.Helper()
	thread := "thread-" + uuid.NewString()[:8]
	x.dispatch(map[string]any{"type": "thread.create", "threadId": thread, "projectId": project, "title": "live", "modelSelection": agent,
		"runtimeMode": "full-access", "interactionMode": "default", "branch": nil, "worktreePath": nil})
	x.dispatch(map[string]any{"type": "thread.turn.start", "threadId": thread, "modelSelection": agent, "runtimeMode": "full-access", "interactionMode": "default",
		"message": map[string]any{"messageId": uuid.NewString(), "role": "user", "text": prompt, "attachments": []any{}}})
	x.t.Logf("T3: %v thread %s started", agent["instanceId"], thread)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		switch state := x.turnState(thread); state {
		case "completed":
			x.t.Logf("T3: %v thread %s completed", agent["instanceId"], thread)
			return
		case "failed", "interrupted", "error":
			x.t.Fatalf("T3's %v thread %s: turn %s\n%s", agent["instanceId"], thread, state, tail(x.out.String(), 3000))
		}
		time.Sleep(time.Second)
	}
	x.t.Fatalf("T3's %v thread %s never settled\n%s", agent["instanceId"], thread, tail(x.out.String(), 3000))
}

// turnState is the state of thread's latest turn in T3's snapshot, "" before it has one.
func (x *T3) turnState(thread string) string {
	var snap any
	if json.Unmarshal(x.request(http.MethodGet, "/api/orchestration/snapshot", nil), &snap) != nil {
		return ""
	}
	var state string
	var walk func(v any)
	walk = func(v any) {
		switch o := v.(type) {
		case map[string]any:
			if o["id"] == thread || o["threadId"] == thread {
				if lt, ok := o["latestTurn"].(map[string]any); ok {
					state, _ = lt["state"].(string)
				}
			}
			for _, e := range o {
				walk(e)
			}
		case []any:
			for _, e := range o {
				walk(e)
			}
		}
	}
	walk(snap)
	return state
}
