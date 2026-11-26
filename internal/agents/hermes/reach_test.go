package hermes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestHermesStartRunAndToolReachCrossOrgMixed(t *testing.T) {
	stateDir := t.TempDir()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	repoA, repoB := filepath.Join(parent, "alpha"), filepath.Join(parent, "beta")
	for _, r := range [][2]string{{repoA, "git@github.com:orgA/alpha.git"}, {repoB, "git@github.com:orgB/beta.git"}} {
		_, _ = gitx.Git(context.Background(), parent, "init", "-q", "-b", "main", r[0])
		_, _ = gitx.Git(context.Background(), r[0], "remote", "add", "origin", r[1])
	}
	fileB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const acct = "1111111111111111"
	now := time.Now().UTC().Truncate(time.Second)
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		for _, spec := range [][3]string{{"orgA", "team-a", "github.com/orgA/alpha"}, {"orgB", "team-b", "github.com/orgB/beta"}} {
			if _, err := s.Select(routing.Team{OrganizationID: spec[0], AuthURL: "https://api.example.com", TeamID: spec[1],
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{spec[2]}, Revision: 1, FetchedAt: now, TeamID: spec[1], OrganizationID: spec[0], AuthURL: "https://api.example.com"}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	sp.Bind(acct)
	rt := hookrun.NewRouting("default", &st)
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{StateDir: stateDir, Cwd: repoA, Spool: sp, Account: acct, Routing: rt, Now: now, Stdin: strings.NewReader(stdin)}
	}
	events := Agent{}.Events()
	ctx := context.Background()

	parentID := "hermes-parent-1"
	childID := "hermes-child-1"
	if err := events["hermes-session-start"](ctx, env(`{"session_id":"`+parentID+`","cwd":`+strconv.Quote(repoA)+`}`)); err != nil {
		t.Fatal(err)
	}
	parentRun, ok := runs.Locate(stateDir, parentID)
	if !ok || parentRun.Run == "" {
		t.Fatalf("hermes-session-start did not start run: %+v (ok=%v)", parentRun, ok)
	}
	if err := events["hermes-session-start"](ctx, env(`{"session_id":"`+childID+`","cwd":`+strconv.Quote(repoA)+`,"parent_session_id":"`+parentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if p, ok := runs.Locate(stateDir, childID); !ok || p.Run != parentRun.Run {
		t.Fatalf("subagent hermes-session-start run = %q (ok=%v); want parent run %q", p.Run, ok, parentRun.Run)
	}

	reaches := []map[string]any{
		{"tool": "read_file", "paths": []string{fileB}},
		{"tool": "search_files", "paths": []string{repoB}, "globs": []string{"*.go"}},
		{"tool": "glob", "globs": []string{"../beta/**/*.go"}},
		{"tool": "terminal", "command": "cat ../beta/secret.go"},
	}
	for i, payload := range reaches {
		sid := "hermes-reach-" + string(rune('1'+i))
		if err := events["hermes-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":`+strconv.Quote(repoA)+`}`)); err != nil {
			t.Fatal(err)
		}
		payload["session_id"] = sid
		payload["cwd"] = repoA
		body, _ := json.Marshal(payload)
		if err := events["hermes-tool-reach"](ctx, env(string(body))); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, now); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("%v: claim = %+v (ok=%v); want KindMixed", payload["tool"], c, ok)
		}
	}
}

func TestHermesRefreshMachineUpdatesInstalledPlugin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERMES_HOME", dir)
	path := filepath.Join(dir, "plugins", "terma", "__init__.py")

	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `"""Terma old hermes plugin"""
CONFIG_JSON = "{\"version\":1,\"endpoint\":\"http://127.0.0.1:43180\",\"headers\":{\"Authorization\":\"Bearer tok\"},\"includePrompts\":true,\"includeToolContent\":true,\"hookCommand\":[\"/custom/terma\",\"hook\"]}"  # terma:config
def register(ctx): pass
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || !changed {
		t.Fatalf("RefreshMachine(outdated) = (%v, %v); want (true, nil)", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`\"/custom/terma\"`, `http://127.0.0.1:43180`, `"hermes-tool-reach"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed plugin missing %s", want)
		}
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}

func TestHermesPluginToolReachContractUnderPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.Mkdir(hooksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nf=$(mktemp "+hooksDir+"/call.XXXXXX)\nprintf '{\"event\":\"%s\",\"payload\":' \"$1\" > $f.part\ncat >> $f.part\necho '}' >> $f.part\nmv $f.part $f.json\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	text, err := renderPlugin(pluginConfig{Endpoint: "http://127.0.0.1:43180", HookCommand: []string{hook}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terma_plugin.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import sys, time
sys.path.insert(0, sys.argv[1])
import terma_plugin as p
p._on_post_tool_call(session_id="s1", tool_name="read_file", args={"path": "/repoB/secret.go"})
p._on_post_tool_call(session_id="s1", tool_name="search_files", args={"target": "content", "path": "/repoB", "file_glob": "*.go"})
p._on_post_tool_call(session_id="s1", tool_name="search_files", args={"target": "files", "pattern": "../repoB/**/*.go"})
p._on_post_tool_call(session_id="s1", tool_name="list_directory", args={"path": "/repoB"})
p._on_post_tool_call(session_id="s1", tool_name="terminal", args={"command": "cat ../repoB/a", "workdir": "/repoB"})
p._on_post_tool_call(session_id="s1", tool_name="write_file", args={"path": "/repoA/a.go"})
time.sleep(0.2)
`
	if out, err := exec.Command(python, "-c", driver, dir).CombinedOutput(); err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	type call struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	var reaches, edits int
	byTool := map[string][]map[string]any{}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		clear(byTool)
		reaches, edits = 0, 0
		files, _ := filepath.Glob(filepath.Join(hooksDir, "*.json"))
		for _, f := range files {
			var c call
			if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &c) == nil {
				switch c.Event {
				case "hermes-tool-reach":
					reaches++
					tool, _ := c.Payload["tool"].(string)
					byTool[tool] = append(byTool[tool], c.Payload)
				case "hermes-file-edit":
					edits++
				}
			}
		}
		if reaches == 5 && edits == 1 || time.Now().After(deadline) {
			break
		}
	}
	if reaches != 5 || edits != 1 {
		t.Fatalf("reaches=%d edits=%d (%v); want reaches=5 edits=1", reaches, edits, byTool)
	}
	if _, ok := byTool["write_file"]; ok {
		t.Fatalf("write_file unexpectedly emitted hermes-tool-reach")
	}
	if term := byTool["terminal"]; len(term) != 1 || term[0]["command"] != "cat ../repoB/a" || term[0]["dir"] != "/repoB" {
		t.Fatalf("terminal reach = %v", term)
	}
}

func TestHermesToolReachFailsClosedOnHookFailureUnderPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()

	var mu sync.Mutex
	var spanNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/traces" {
			var req struct {
				ResourceSpans []struct {
					ScopeSpans []struct {
						Spans []struct {
							Name string `json:"name"`
						} `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			}
			if json.Unmarshal(body, &req) == nil {
				mu.Lock()
				for _, rs := range req.ResourceSpans {
					for _, ss := range rs.ScopeSpans {
						for _, sp := range ss.Spans {
							spanNames = append(spanNames, sp.Name)
						}
					}
				}
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	failStart := filepath.Join(dir, "fail-start")
	if err := os.WriteFile(failStart, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "hook.sh")
	hookSrc := "#!/bin/sh\n" +
		"if [ \"$1\" = \"hermes-tool-reach\" ]; then exit 1; fi\n" +
		"if [ \"$1\" = \"hermes-session-start\" ] && [ -f " + strconv.Quote(failStart) + " ]; then exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	text, err := renderPlugin(pluginConfig{Endpoint: srv.URL, Headers: map[string]string{}, HookCommand: []string{hook}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terma_plugin.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import os, sys, threading
sys.path.insert(0, sys.argv[1])
import terma_plugin as p
# 1. Reach hook exits non-zero -> fails closed and taints s1 (drops read_file AND follow-up spans).
p._on_post_tool_call(session_id="s1", tool_name="read_file", args={"path": "/repoB/secret.go"})
p._on_post_tool_call(session_id="s1", tool_name="write_file", args={"path": "/repoA/after_taint.go"})
# 1b. Resumed session start that fails keeps s1 tainted.
p._on_session_start(session_id="s1")
p._on_post_tool_call(session_id="s1", tool_name="patch", args={"path": "/repoA/still_tainted.go"})
# 1c. Resumed session start that succeeds clears taint so subsequent spans emit again.
os.remove(sys.argv[2])
p._on_session_start(session_id="s1")
p._on_post_tool_call(session_id="s1", tool_name="write_file", args={"path": "/repoA/resumed.go"})
# 2. Separate clean session still emits execute_tool write_file span.
p._on_post_tool_call(session_id="s_clean", tool_name="write_file", args={"path": "/repoA/a.go"})
# 3. Overflowing the bounded hook queue also fails closed for reach tools.
p._hook_worker_started = True
while not p._hook_queue.full():
    p._hook_queue.put_nowait(("noop", {}, threading.Event(), [False]))
p._on_post_tool_call(session_id="s2", tool_name="terminal", args={"command": "cat ../repoB/secret.go"})
p._flush()
`
	if out, err := exec.Command(python, "-c", driver, dir, failStart).CombinedOutput(); err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	var writeCount int
	for _, name := range spanNames {
		if name == "execute_tool read_file" || name == "execute_tool terminal" || name == "execute_tool patch" {
			t.Fatalf("failed/tainted session leaked span %q (all=%v)", name, spanNames)
		}
		if name == "execute_tool write_file" {
			writeCount++
		}
	}
	if writeCount != 2 {
		t.Fatalf("expected 2 execute_tool write_file spans (s1 after resumed start + s_clean); got %v", spanNames)
	}
}

func TestHermesHookWorkerWaitsForConfirmedExitUnderPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	text, err := renderPlugin(pluginConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{}, HookCommand: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terma_plugin.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import json, subprocess, sys, threading, time
sys.path.insert(0, sys.argv[1])
import terma_plugin as p

started = []
release = threading.Event()
second_started = threading.Event()

class StubProc:
    def __init__(self, event):
        self.event = event
        self.returncode = None
        started.append(event)
        if event == "second":
            second_started.set()
    def communicate(self, data, timeout=None):
        if self.event == "first":
            raise subprocess.TimeoutExpired(cmd="first", timeout=timeout)
        self.returncode = 0
        return (b"", b"")
    def poll(self):
        return self.returncode
    def kill(self):
        if not release.is_set():
            raise OSError("transient kill error")
        self.returncode = -9
    def wait(self, timeout=None):
        if not release.wait(timeout=min(timeout or 0.1, 0.05)):
            raise subprocess.TimeoutExpired(cmd=self.event, timeout=timeout)
        self.returncode = -9
        return -9

p.subprocess.Popen = lambda cmd, **kw: StubProc(cmd[-1])
ok1 = p._terma_hook("first", {}, wait=True)
p._terma_hook("second", {}, wait=False)
time.sleep(0.2)
started_while_alive = second_started.is_set()
release.set()
started_after_exit = second_started.wait(timeout=2.0)
print(json.dumps({"ok1": ok1, "started_while_alive": started_while_alive, "started_after_exit": started_after_exit, "started": started}))
`
	out, err := exec.Command(python, "-c", driver, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	var got struct {
		OK1               bool     `json:"ok1"`
		StartedWhileAlive bool     `json:"started_while_alive"`
		StartedAfterExit  bool     `json:"started_after_exit"`
		Started           []string `json:"started"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse output %q: %v", out, err)
	}
	if got.OK1 || got.StartedWhileAlive || !got.StartedAfterExit {
		t.Fatalf("Hermes FIFO confirmed-exit invariant violated: %+v", got)
	}
}
