package e2e

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
	"golang.org/x/net/websocket"
)

// T3 Code (pingdotgg/t3code, `t3` on npm) is a local server with a web front end that
// runs other agents: `codex app-server` (one per T3 thread, in the thread's workspace)
// and Claude Code through the Agent SDK, each the one on PATH, with T3's own
// environment — it sets no OTEL_* of its own (0.0.42, 0.1.0) and, on 0.1.0, passes
// Claude no setting sources, which loads the user's, the project's and the local ones
// as Claude does anywhere. So the agents' user-level exporters and the repository's
// hooks work as they do anywhere, and the relay needs nothing T3-specific; these
// scenarios prove it. T3 is driven the way its front end drives it, with a session
// token from `t3 auth session issue`. Up to 0.0.x that is commands POSTed to
// /api/orchestration/dispatch and state read from /api/orchestration/snapshot. 0.1.0
// answers that snapshot route with its web app instead: projects are POSTed to
// /api/projects/mutate, threads launched over its WebSocket (Effect RPC, JSON), and
// state read from /api/orchestration/shell. A build that speaks neither is skipped as
// one the driver cannot drive (SkipBuild), not failed.

func forEachT3(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	path, err := exec_LookPath("t3")
	if err != nil {
		Record(t.Name(), "not run", "t3 is not installed")
		t.Skip("t3 is not installed")
	}
	b := Binary{Harness: "t3", Version: t3Version(path), Path: path, Installed: true}
	t.Run(b.Label(), func(t *testing.T) { scenarioOf(t, b.Harness); run(t, b) })
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
	v2    bool // 0.1.0's API: /api/projects, the WebSocket, /api/orchestration/shell
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
	x.env = append(append(sb.termaEnv(), "SHELL="+sb.t3Shell(), "T3CODE_HOME="+x.base, "T3CODE_TELEMETRY_ENABLED=false"), env...)
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
	// 0.1.0 answers the old snapshot route with its web app, and serves its own shell
	// snapshot instead. A build that answers the shell route with its web app or a 404
	// too speaks neither API. Any other answer (an error, a refusal, a server that never
	// came up) fails the scenario, here or in the old API, as it should.
	if x.servesWebApp("/api/orchestration/snapshot") {
		if !x.servesShell() {
			SkipBuild(t, "the test driver can't drive this version (it serves neither /api/orchestration/snapshot nor /api/orchestration/shell)")
		}
		x.v2 = true
	}
	return x
}

// t3Shell writes the shell T3 is given as $SHELL and returns its path. T3 puts the PATH
// of `$SHELL -ilc` ahead of its own, and a login shell on macOS puts /etc/paths ahead of
// the sandbox's, so T3 would run the developer's `codex` and `claude`, not the builds
// under test. This one runs the command without a login, keeping the sandbox's PATH.
func (sb *Sandbox) t3Shell() string {
	path := filepath.Join(sb.Dir, "t3-shell")
	script := "#!/bin/sh\n# T3 runs `$SHELL -ilc CMD`: run CMD alone, with no login profile.\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		sb.T.Fatal(err)
	}
	return path
}

// servesWebApp says whether T3 answers a GET of path with its web app's HTML, as a build
// that no longer serves path as an API route does.
func (x *T3) servesWebApp(path string) bool {
	req, _ := http.NewRequest(http.MethodGet, x.url+path, nil)
	req.Header.Set("Authorization", "Bearer "+x.token)
	resp, err := t3Client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode/100 == 2 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html")
}

// servesShell says whether T3 serves 0.1.0's shell snapshot route: true where it answers
// with JSON, false where it answers with its web app or a 404, as a build without the
// route does. Anything else fails the scenario, once a server error or a dropped
// connection has had a few seconds to clear: a build that cannot answer is broken, not
// one the driver cannot drive.
func (x *T3) servesShell() bool {
	x.t.Helper()
	var last string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		req, _ := http.NewRequest(http.MethodGet, x.url+"/api/orchestration/shell", nil)
		req.Header.Set("Authorization", "Bearer "+x.token)
		req.Header.Set(t3ProtocolHeader, t3Protocol)
		resp, err := t3Client.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		_ = resp.Body.Close()
		kind := resp.Header.Get("Content-Type")
		switch {
		case resp.StatusCode/100 == 2 && strings.HasPrefix(kind, "application/json"):
			return true
		case resp.StatusCode/100 == 2 && strings.HasPrefix(kind, "text/html"), resp.StatusCode == http.StatusNotFound:
			return false
		case resp.StatusCode/100 == 5:
			last = resp.Status + " " + string(body)
			continue
		}
		x.t.Fatalf("t3 GET /api/orchestration/shell: %s (%s) %s\n%s", resp.Status, kind, body, tail(x.out.String(), 2000))
	}
	x.t.Fatalf("t3 GET /api/orchestration/shell: %s\n%s", last, tail(x.out.String(), 2000))
	return false
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
	return x.requestWith(method, path, body, nil)
}

func (x *T3) requestWith(method, path string, body any, header http.Header) []byte {
	x.t.Helper()
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, x.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+x.token)
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
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
	if x.v2 {
		x.request(http.MethodPost, "/api/projects/mutate", map[string]any{"type": "project.create", "commandId": uuid.NewString(),
			"projectId": id, "title": filepath.Base(dir), "workspaceRoot": dir})
		return id
	}
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
	if x.v2 {
		x.turnV2(project, thread, agent, prompt)
		return
	}
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

// The header and the WebSocket query parameter that name the orchestration protocol a
// 0.1.0 client speaks; a server answers a client of another with an error.
const (
	t3ProtocolHeader = "x-t3-orchestration-protocol"
	t3Protocol       = "2"
)

// turnV2 is Turn on 0.1.0: the thread launched with its first message in one command, as
// the front end launches a new thread, then T3's shell snapshot polled until the
// thread's latest run has ended. Anything but "completed" fails the scenario.
func (x *T3) turnV2(project, thread string, agent map[string]any, prompt string) {
	x.t.Helper()
	x.rpc("orchestration.launchThread", map[string]any{"commandId": uuid.NewString(), "creationSource": "web", "threadId": thread,
		"projectId": project, "title": "live", "modelSelection": agent, "runtimeMode": "full-access", "interactionMode": "default",
		"workspaceStrategy": map[string]any{"type": "root"},
		"initialMessage":    map[string]any{"messageId": uuid.NewString(), "text": prompt, "attachments": []any{}}})
	x.t.Logf("T3: %v thread %s started", agent["instanceId"], thread)
	seen := "no such thread in T3's shell snapshot"
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if th, ok := x.shellThread(thread); ok {
			if th.LatestRunID != nil && th.ActiveRunID == nil {
				if th.Status != "completed" {
					x.t.Fatalf("T3's %v thread %s: run %s: %s (%s)\n%s", agent["instanceId"], thread, th.Status, th.LastError, th.LastErrorClass, tail(x.out.String(), 3000))
				}
				x.t.Logf("T3: %v thread %s completed", agent["instanceId"], thread)
				return
			}
			seen = fmt.Sprintf("status %s, latest run %s, active run %s", th.Status, deref(th.LatestRunID), deref(th.ActiveRunID))
		}
		time.Sleep(time.Second)
	}
	x.t.Fatalf("T3's %v thread %s never settled (last seen: %s)\n%s", agent["instanceId"], thread, seen, tail(x.out.String(), 3000))
}

// deref is *s, or "none" where s is nil.
func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

// t3ShellThread is what turnV2 reads of a thread in 0.1.0's shell snapshot.
type t3ShellThread struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	LatestRunID *string `json:"latestRunId"`
	ActiveRunID *string `json:"activeRunId"`
	// Raw, as T3 sends them: what fails the scenario is shown, never what it could not decode.
	LastError      json.RawMessage `json:"lastError"`
	LastErrorClass json.RawMessage `json:"lastErrorClass"`
}

// shellThread is thread in T3's shell snapshot, false while the snapshot has no such thread.
func (x *T3) shellThread(thread string) (t3ShellThread, bool) {
	x.t.Helper()
	var snap struct {
		Threads []t3ShellThread `json:"threads"`
	}
	data := x.requestWith(http.MethodGet, "/api/orchestration/shell", nil, http.Header{t3ProtocolHeader: {t3Protocol}})
	if err := json.Unmarshal(data, &snap); err != nil {
		x.t.Fatalf("t3 shell snapshot: %v\n%s", err, tail(string(data), 2000))
	}
	for _, th := range snap.Threads {
		if th.ID == thread {
			return th, true
		}
	}
	return t3ShellThread{}, false
}

// rpc makes one Effect RPC call over 0.1.0's WebSocket and returns its result: one
// connection per call, authenticated by the session token on the upgrade, its request
// a JSON message and its answer the Exit that names the request. A failed call fails
// the scenario with its cause. The client pings in Effect RPC and the server only
// answers, so a call this short has nothing to answer but its Exit.
func (x *T3) rpc(tag string, payload any) json.RawMessage {
	x.t.Helper()
	cfg, err := websocket.NewConfig(strings.Replace(x.url, "http", "ws", 1)+"/ws?orchestrationProtocol="+t3Protocol, x.url)
	if err != nil {
		x.t.Fatal(err)
	}
	cfg.Header.Set("Authorization", "Bearer "+x.token)
	// The upgrade is bounded as every request is, not only the connect.
	ctx, cancel := context.WithTimeout(context.Background(), t3Client.Timeout)
	defer cancel()
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		x.t.Fatalf("t3 %s: %v\n%s", tag, err, tail(x.out.String(), 2000))
	}
	defer func() { _ = ws.Close() }()
	_ = ws.SetDeadline(time.Now().Add(t3Client.Timeout))
	const id = "1" // the connection's only request
	req := map[string]any{"_tag": "Request", "id": id, "tag": tag, "payload": payload, "headers": []any{}}
	if err := websocket.JSON.Send(ws, req); err != nil {
		x.t.Fatalf("t3 %s: %v", tag, err)
	}
	for {
		var msg struct {
			Tag       string          `json:"_tag"`
			RequestID string          `json:"requestId"`
			Defect    json.RawMessage `json:"defect"`
			Exit      struct {
				Tag   string          `json:"_tag"`
				Value json.RawMessage `json:"value"`
				Cause json.RawMessage `json:"cause"`
			} `json:"exit"`
		}
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			x.t.Fatalf("t3 %s: no answer: %v\n%s", tag, err, tail(x.out.String(), 2000))
		}
		switch {
		case msg.Tag == "Exit" && msg.RequestID == id:
			if msg.Exit.Tag != "Success" {
				x.t.Fatalf("t3 %s: %s %s", tag, msg.Exit.Tag, tail(string(msg.Exit.Cause), 2000))
			}
			return msg.Exit.Value
		case msg.Tag == "Defect":
			x.t.Fatalf("t3 %s: defect: %s\n%s", tag, tail(string(msg.Defect), 2000), tail(x.out.String(), 2000))
		}
	}
}
