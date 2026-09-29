package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Codex Desktop — and the ChatGPT VS Code extension — run ONE `codex app-server` for every
// workspace, so one process exports every repository's threads. These scenarios drive
// that shape headlessly over the app-server's stdio JSON-RPC (the protocol Desktop
// speaks: initialize, thread/start with a cwd, turn/start, thread/resume) and check the
// relay routes each thread by its own directory, from the rollouts alone: the repository
// hooks are left untrusted, as a Desktop user's often are.

// appServer is one running `codex app-server` and a minimal client for it.
type appServer struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	nextID atomic.Int64

	mu        sync.Mutex
	pending   map[int64]chan rpcReply
	completed map[string]int // threadId → turns completed
	changed   chan struct{}
	stderr    strings.Builder
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// startAppServer runs `codex app-server` in the sandbox the way Codex Desktop runs its
// bundled one: the machine's global configuration (pointed at the relay by UseRelay), a
// fake model provider, no plugins.
func (sb *Sandbox) startAppServer(providerURL string) *appServer {
	t := sb.T
	t.Helper()
	sb.prepareCodex(RouteAPIKey)
	args := []string{"app-server", "-c", `cli_auth_credentials_store="file"`,
		"-c", "features.plugins=false", "-c", "features.remote_plugin=false", "-c", `model_reasoning_effort="low"`}
	fixture := fixtureCodexArgs(providerURL)
	for i := 0; i+1 < len(fixture); i += 2 {
		if fixture[i] == "-c" { // app-server takes config overrides, not exec's -s
			args = append(args, fixture[i], fixture[i+1])
		}
	}
	cmd := exec.Command(sb.Codex.Path, args...)
	cmd.Dir = sb.Repo
	cmd.Env = sb.codexEnv(RouteAPIKey)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &appServer{t: t, cmd: cmd, stdin: stdin, pending: map[int64]chan rpcReply{},
		completed: map[string]int{}, changed: make(chan struct{}, 1)}
	cmd.Stderr = &lockedWriter{mu: &s.mu, b: &s.stderr}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start codex app-server: %v", err)
	}
	go s.read(stdout)
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			s.mu.Lock()
			t.Logf("codex app-server stderr:\n%s", s.stderr.String())
			s.mu.Unlock()
		}
	})
	s.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "terma_live", "title": nil, "version": "0.0.0"},
		"capabilities": map[string]any{"experimentalApi": true, "requestAttestation": false},
	})
	s.notify("initialized", nil)
	return s
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (s *appServer) send(msg map[string]any) {
	data, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		s.t.Fatalf("write to app-server: %v", err)
	}
}

func (s *appServer) notify(method string, params any) {
	msg := map[string]any{"method": method}
	if params != nil {
		msg["params"] = params
	}
	s.send(msg)
}

// call sends a request and waits for its answer.
func (s *appServer) call(method string, params any) json.RawMessage {
	s.t.Helper()
	id := s.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	s.send(map[string]any{"id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		if r.Error != nil {
			s.t.Fatalf("%s: %d %s", method, r.Error.Code, r.Error.Message)
		}
		return r.Result
	case <-time.After(60 * time.Second):
		s.t.Fatalf("%s: no answer from the app-server", method)
		return nil
	}
}

// read dispatches answers to their calls, counts completed turns, and declines any
// request the server makes (approvals are "never" asked; nothing else is expected).
func (s *appServer) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			rpcReply
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		switch {
		case msg.ID != nil && msg.Method == "":
			s.mu.Lock()
			ch := s.pending[*msg.ID]
			delete(s.pending, *msg.ID)
			s.mu.Unlock()
			if ch != nil {
				ch <- msg.rpcReply
			}
		case msg.ID != nil:
			s.send(map[string]any{"id": *msg.ID, "error": map[string]any{"code": -32601, "message": "not handled by the test client"}})
		case msg.Method == "turn/completed":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.mu.Lock()
			s.completed[p.ThreadID]++
			s.mu.Unlock()
			select {
			case s.changed <- struct{}{}:
			default:
			}
		}
	}
}

// startThread opens a thread in cwd, as Desktop does when a workspace's task starts.
func (s *appServer) startThread(cwd string) string {
	s.t.Helper()
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	raw := s.call("thread/start", map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": "workspace-write"})
	if err := json.Unmarshal(raw, &r); err != nil || r.Thread.ID == "" {
		s.t.Fatalf("thread/start in %s: %s", cwd, raw)
	}
	return r.Thread.ID
}

// resumeThread reopens thread id in cwd, as a developer continuing it from another
// workspace would.
func (s *appServer) resumeThread(id, cwd string) {
	s.t.Helper()
	s.call("thread/resume", map[string]any{"threadId": id, "cwd": cwd, "approvalPolicy": "never", "sandbox": "workspace-write", "excludeTurns": true})
}

// turn starts a turn in thread id; the turn's completion is awaited with waitTurns. A
// cwd, when given, is the directory the turn runs in (turn/start's own override).
func (s *appServer) turn(id, prompt string, cwd ...string) {
	s.t.Helper()
	params := map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": prompt, "text_elements": []any{}}}}
	if len(cwd) > 0 {
		params["cwd"] = cwd[0]
	}
	s.call("turn/start", params)
}

// waitTurns waits until every thread in want has completed that many turns.
func (s *appServer) waitTurns(want map[string]int) {
	s.t.Helper()
	deadline := time.After(scenarioTimeout)
	for {
		s.mu.Lock()
		done := true
		for id, n := range want {
			if s.completed[id] < n {
				done = false
			}
		}
		got := fmt.Sprint(s.completed)
		s.mu.Unlock()
		if done {
			return
		}
		select {
		case <-s.changed:
		case <-time.After(time.Second):
		case <-deadline:
			s.t.Fatalf("turns did not complete: got %s, want %v", got, want)
		}
	}
}

// replyProvider answers every model request with a plain reply: the scenarios are about
// where each thread's records go, and several threads share one provider.
func replyProvider(t *testing.T) *httptest.Server {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		writeCodexResponse(w, calls.Add(1), map[string]any{"type": "message", "id": "msg_appserver", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "TERMA_APPSERVER_REPLY", "annotations": []any{}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// One app-server, three workspaces at once: a repository bound to one project, another
// bound to a second, and a personal directory. Every record of each thread reaches its
// own project — the personal one's the machine project — and none reaches another.
func TestRelayCodexAppServerRoutesEachWorkspace(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.CodexHooksUntrusted = true
		sb.UseRelay(RelayOptions{Content: true})
		other := sb.InstallOther()
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		srv := sb.startAppServer(replyProvider(t).URL)

		type workspace struct {
			name, dir, key, marker string
			thread                 string
		}
		spaces := []*workspace{
			{name: "the bound repository", dir: sb.Repo, key: liveKey, marker: "TERMA_WS_BOUND"},
			{name: "the other repository", dir: other, key: otherKey, marker: "TERMA_WS_OTHER"},
			{name: "the personal directory", dir: personal, key: machineKey, marker: "TERMA_WS_PERSONAL"},
		}
		for _, w := range spaces {
			w.thread = srv.startThread(w.dir)
		}
		// Interleaved, as Desktop's tasks run: every turn starts before any completes.
		want := map[string]int{}
		for _, w := range spaces {
			srv.turn(w.thread, w.marker+" please reply")
			want[w.thread] = 1
		}
		srv.waitTurns(want)

		arrived := func() bool {
			for _, w := range spaces {
				if len(leakedFieldsOf(sb.Receiver.evidenceFor(bearer(w.key)), w.marker)) == 0 {
					return false
				}
			}
			return true
		}
		if !waitFor(relayHold+20*time.Second, arrived) {
			t.Errorf("not every workspace's prompt arrived at its project")
		}
		for _, w := range spaces {
			for _, elsewhere := range spaces {
				if elsewhere == w {
					continue
				}
				if n := len(leakedFieldsOf(sb.Receiver.evidenceFor(bearer(elsewhere.key)), w.marker)); n != 0 {
					t.Errorf("%s's prompt reached %s's project in %d fields", w.name, elsewhere.name, n)
				}
				if n := agentSessions(sb.Receiver.evidenceFor(bearer(elsewhere.key)))[w.thread]; n != 0 {
					t.Errorf("%d records of %s's thread reached %s's project", n, w.name, elsewhere.name)
				}
			}
			if n := agentSessions(sb.Receiver.evidenceFor(bearer(w.key)))[w.thread]; n == 0 {
				t.Errorf("no record of %s's thread reached its project", w.name)
			}
		}
		checkNoRepositoryHookRan(t, sb)
		if t.Failed() {
			for _, w := range spaces {
				t.Logf("%s (thread %s):\n%s", w.name, w.thread, sb.routingReport(w.thread))
			}
		}
		noteRelay(t.Name(), sb)
	})
}

// The same process resumes a thread in another workspace: the resumed turn follows its
// new directory, and the first turn stays with the repository it ran in.
func TestRelayCodexAppServerResumeElsewhere(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.CodexHooksUntrusted = true
		sb.UseRelay(RelayOptions{Content: true})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		srv := sb.startAppServer(replyProvider(t).URL)
		thread := srv.startThread(sb.Repo)
		srv.turn(thread, "TERMA_FIRST_TURN please reply")
		srv.waitTurns(map[string]int{thread: 1})
		// A loaded thread keeps its directory through thread/resume (0.158: its next
		// turn_context still names the repository); a turn moves it with its own cwd.
		srv.resumeThread(thread, personal)
		srv.turn(thread, "TERMA_RESUMED_TURN please reply", personal)
		srv.waitTurns(map[string]int{thread: 2})

		waitFor(relayHold+20*time.Second, func() bool {
			return len(leakedFieldsOf(sb.Receiver.evidence(), "TERMA_RESUMED_TURN")) > 0 &&
				len(leakedFieldsOf(sb.Receiver.evidence(), "TERMA_FIRST_TURN")) > 0
		})
		first := func(key string) int {
			return len(leakedFieldsOf(sb.Receiver.evidenceFor(bearer(key)), "TERMA_FIRST_TURN"))
		}
		resumed := func(key string) int {
			return len(leakedFieldsOf(sb.Receiver.evidenceFor(bearer(key)), "TERMA_RESUMED_TURN"))
		}
		msg := fmt.Sprintf("first turn: bound %d, machine %d; resumed turn: bound %d, machine %d",
			first(liveKey), first(machineKey), resumed(liveKey), resumed(machineKey))
		t.Log(msg)
		t.Logf("rollout directories: %s", rolloutDirs(t, sb, thread))
		if first(liveKey) == 0 || first(machineKey) != 0 || resumed(machineKey) == 0 || resumed(liveKey) != 0 {
			t.Errorf("each turn should reach only the project of the directory it ran in: %s\n%s", msg, sb.routingReport(thread))
		}
		checkNoRepositoryHookRan(t, sb)
		noteRelay(t.Name(), sb)
	})
}

// checkNoRepositoryHookRan fails when a Codex repository hook ran: the scenario would not
// prove the rollouts placed the threads.
func checkNoRepositoryHookRan(t *testing.T, sb *Sandbox) {
	t.Helper()
	entries, _ := os.ReadDir(sb.payloadDir())
	for _, e := range entries {
		if strings.Contains(e.Name(), "codex-session-start") {
			t.Errorf("a repository hook ran (%s), so this does not prove the rollouts placed the threads", e.Name())
		}
	}
}

// rolloutDirs lists the directory-bearing records of thread's rollout, as the relay reads
// them: the header, thread_settings_applied, turn_context.
func rolloutDirs(t *testing.T, sb *Sandbox, thread string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(sb.CodexHome, "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if len(matches) == 0 {
		return "no rollout"
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		var rec struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type           string `json:"type"`
				Cwd            string `json:"cwd"`
				ThreadSettings struct {
					Cwd string `json:"cwd"`
				} `json:"thread_settings"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		cwd := rec.Payload.Cwd
		if rec.Payload.Type == "thread_settings_applied" {
			cwd = rec.Payload.ThreadSettings.Cwd
		}
		if cwd != "" || rec.Payload.Type == "task_started" {
			fmt.Fprintf(&b, "\n  %s %s/%s %s", rec.Timestamp, rec.Type, rec.Payload.Type, cwd)
		}
	}
	return b.String()
}
