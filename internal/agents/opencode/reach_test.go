package opencode

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

func TestOpenCodeStartRunAndToolReachCrossOrgMixed(t *testing.T) {
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

	parentID := "ses_parent_oc"
	childID := "ses_child_oc"
	if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+parentID+`","cwd":`+strconv.Quote(repoA)+`}`)); err != nil {
		t.Fatal(err)
	}
	parentRun, ok := runs.Locate(stateDir, parentID)
	if !ok || parentRun.Run == "" {
		t.Fatalf("opencode-session-start did not start run: %+v (ok=%v)", parentRun, ok)
	}
	if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+childID+`","cwd":`+strconv.Quote(repoA)+`,"parent_session_id":"`+parentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if p, ok := runs.Locate(stateDir, childID); !ok || p.Run != parentRun.Run {
		t.Fatalf("subagent opencode-session-start run = %q (ok=%v); want parent run %q", p.Run, ok, parentRun.Run)
	}

	reaches := []map[string]any{
		{"tool": "read", "paths": []string{fileB}},
		{"tool": "grep", "paths": []string{repoB}, "globs": []string{"*.go"}},
		{"tool": "glob", "globs": []string{"../beta/**/*.go"}},
		{"tool": "bash", "command": "cat ../beta/secret.go"},
	}
	for i, payload := range reaches {
		sid := "ses_reach_" + string(rune('a'+i))
		if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":`+strconv.Quote(repoA)+`}`)); err != nil {
			t.Fatal(err)
		}
		payload["session_id"] = sid
		payload["cwd"] = repoA
		body, _ := json.Marshal(payload)
		if err := events["opencode-tool-reach"](ctx, env(string(body))); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, now); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("%v: claim = %+v (ok=%v); want KindMixed", payload["tool"], c, ok)
		}
	}
}

// The plugin checks a write or an edit on the file it changes before the tool runs. A file
// that does not exist yet inside another organization's checkout crosses into it: the run
// goes Mixed at the check, before the edit's span can leave.
func TestOpenCodeEditReachToANewFileCrossOrgMixed(t *testing.T) {
	stateDir := t.TempDir()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	repoA, repoB := filepath.Join(parent, "alpha"), filepath.Join(parent, "beta")
	for _, r := range [][2]string{{repoA, "git@github.com:orgA/alpha.git"}, {repoB, "git@github.com:orgB/beta.git"}} {
		_, _ = gitx.Git(context.Background(), parent, "init", "-q", "-b", "main", r[0])
		_, _ = gitx.Git(context.Background(), r[0], "remote", "add", "origin", r[1])
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

	for _, tool := range []string{"write", "edit"} {
		sid := "ses_" + tool + "_new_oc"
		if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":`+strconv.Quote(repoA)+`}`)); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, now); ok && c.Kind == claim.KindMixed {
			t.Fatalf("%s: claim is Mixed before any reach: %+v", tool, c)
		}
		newFile := filepath.Join(repoB, "pkg", tool+".go")
		if _, err := os.Stat(newFile); !os.IsNotExist(err) {
			t.Fatalf("%s exists before the %s: %v", newFile, tool, err)
		}
		body, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoA, "tool": tool, "paths": []string{newFile}})
		if err := events["opencode-tool-reach"](ctx, env(string(body))); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, now); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("%s of a new file in another organization's checkout: claim = %+v (ok=%v); want KindMixed", tool, c, ok)
		}
	}
}

// An attached file OpenCode gives no path for is told to terma as a read it cannot place,
// under a path that starts with two separators. That must stay an unknown reach: the run
// goes Mixed, as for a shell command terma cannot follow.
func TestOpenCodeUnplacedAttachmentMarksTheRunMixed(t *testing.T) {
	stateDir := t.TempDir()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(parent, "alpha")
	_, _ = gitx.Git(context.Background(), parent, "init", "-q", "-b", "main", repo)
	_, _ = gitx.Git(context.Background(), repo, "remote", "add", "origin", "git@github.com:orgA/alpha.git")
	const acct = "1111111111111111"
	now := time.Now().UTC().Truncate(time.Second)
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		if _, err := s.Select(routing.Team{OrganizationID: "orgA", AuthURL: "https://api.example.com", TeamID: "team-a",
			Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/orgA/alpha"}, Revision: 1, FetchedAt: now, TeamID: "team-a", OrganizationID: "orgA", AuthURL: "https://api.example.com"}}); err != nil {
			return err
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
		return hookrun.Env{StateDir: stateDir, Cwd: repo, Spool: sp, Account: acct, Routing: rt, Now: now, Stdin: strings.NewReader(stdin)}
	}
	events := Agent{}.Events()
	ctx := context.Background()

	const sid = "ses_attached_oc"
	if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":`+strconv.Quote(repo)+`}`)); err != nil {
		t.Fatal(err)
	}
	if why := runs.Read(stateDir, sid).Why(now.Add(time.Minute)); why.Reason != "" {
		t.Fatalf("run is Mixed before any reach: %+v", why)
	}
	body, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repo, "tool": "read", "paths": []string{"//attached-file/unplaced"}})
	if err := events["opencode-tool-reach"](ctx, env(string(body))); err != nil {
		t.Fatal(err)
	}
	if c, ok := claim.Read(stateDir, sid, now); !ok || c.Kind != claim.KindMixed {
		t.Fatalf("claim = %+v (ok=%v); want KindMixed", c, ok)
	}
	if why := runs.Read(stateDir, sid).Why(now.Add(time.Minute)); why.Reason != runs.UnknownReach {
		t.Fatalf("Mixed reason = %q (%+v); want %q", why.Reason, why, runs.UnknownReach)
	}
}

func TestOpenCodeRefreshConnectDisconnectConcurrentLockSafety(t *testing.T) {
	h, path := opencodeIn(t)
	e := opencodeExporter(t, h, true)
	if err := h.Connect(e, false); err != nil {
		t.Fatalf("initial connect: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for range 3 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := h.Connect(e, false); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				if _, _, err := h.RefreshPlugin(); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := h.Disconnect(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent opencode connect/refresh/disconnect error: %v", err)
	}
	if _, err := h.Disconnect(); err != nil {
		t.Fatalf("final disconnect: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plugin still present after final disconnect: err=%v", err)
	}
}

func TestOpenCodePluginToolReachContractUnderNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	src, err := renderPlugin(opencodeConfig{
		Version:     1,
		Endpoint:    srv.URL,
		Signals:     []string{"traces"},
		HookCommand: []string{hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(dir, "terma.mjs")
	if err := os.WriteFile(pluginPath, src, 0o600); err != nil {
		t.Fatal(err)
	}
	// The read's tool part repeats what its guard saw, so it calls no second reach hook.
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const hooks = await mod.default.server({ directory: "/repoA", worktree: "/repoA" })
` + opencodeDriverHelpers + `
await hooks.event(created("ses_oc"))
await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_oc", callID: "c1" }, { args: { filePath: "/repoB/secret.go" } })
await hooks.event(toolDone("ses_oc", "c1", "read", { filePath: "/repoB/secret.go" }, "ok"))
await hooks["tool.execute.before"]({ tool: "grep", sessionID: "ses_oc", callID: "c2" }, { args: { path: "/repoB", include: "*.go" } })
await hooks["tool.execute.before"]({ tool: "glob", sessionID: "ses_oc", callID: "c3" }, { args: { pattern: "../repoB/**/*.go" } })
await hooks["tool.execute.before"]({ tool: "list", sessionID: "ses_oc", callID: "c4" }, { args: { path: "/repoB" } })
await hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_oc", callID: "c5" }, { args: { command: "cat ../repoB/a", workdir: "/repoB" } })
await hooks["tool.execute.before"]({ tool: "edit", sessionID: "ses_oc", callID: "c6" }, { args: { filePath: "/repoA/x.go" } })
await hooks.event(toolDone("ses_oc", "c6", "edit", { filePath: "/repoA/x.go" }, "ok"))
await hooks.dispose()
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, pluginPath).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	type call struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	byTool := map[string]map[string]any{}
	var reaches int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		clear(byTool)
		reaches = 0
		files, _ := filepath.Glob(filepath.Join(hooksDir, "*.json"))
		for _, f := range files {
			var c call
			if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &c) == nil && c.Event == "opencode-tool-reach" {
				reaches++
				if tool, _ := c.Payload["tool"].(string); tool != "" {
					byTool[tool] = c.Payload
				}
			}
		}
		if len(byTool) == 6 || time.Now().After(deadline) {
			break
		}
	}
	if reaches != 6 {
		t.Fatalf("opencode-tool-reach calls = %d (%v); want 6 (deduped across the guard and the tool part)", reaches, byTool)
	}
	if paths, _ := byTool["edit"]["paths"].([]any); len(paths) != 1 || paths[0] != "/repoA/x.go" {
		t.Fatalf("edit reach = %v; want the file it changes", byTool["edit"])
	}
	if byTool["bash"]["command"] != "cat ../repoB/a" || byTool["bash"]["dir"] != "/repoB" {
		t.Fatalf("bash reach = %v", byTool["bash"])
	}
}

func TestOpenCodeHookRunnerKillsTimedOutChildBeforeNextHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "hung.pid")
	statusFile := filepath.Join(dir, "reach.status")
	hook := filepath.Join(dir, "hook.sh")
	hookSrc := "#!/bin/sh\n" +
		"if [ \"$1\" = \"opencode-session-start\" ]; then\n" +
		"  echo $$ > " + strconv.Quote(pidFile) + "\n" +
		"  exec sleep 30\n" +
		"fi\n" +
		"if [ \"$1\" = \"opencode-tool-reach\" ]; then\n" +
		"  pid=$(cat " + strconv.Quote(pidFile) + " 2>/dev/null || true)\n" +
		"  if [ -n \"$pid\" ] && kill -0 \"$pid\" 2>/dev/null; then\n" +
		"    echo alive > " + strconv.Quote(statusFile) + "\n" +
		"  else\n" +
		"    echo dead > " + strconv.Quote(statusFile) + "\n" +
		"  fi\n" +
		"fi\n"
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := renderPlugin(opencodeConfig{
		Version:     1,
		Endpoint:    "http://127.0.0.1:43180",
		Signals:     []string{"traces"},
		HookCommand: []string{hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(dir, "terma.mjs")
	if err := os.WriteFile(pluginPath, src, 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const hooks = await mod.default.server({ directory: "/repoA", worktree: "/repoA" })
` + opencodeDriverHelpers + `
hooks.event(created("ses_hang"))
await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_hang", callID: "c1" }, { args: { filePath: "/repoB/secret.go" } })
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, pluginPath).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	got, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("read statusFile: %v", err)
	}
	if strings.TrimSpace(string(got)) != "dead" {
		t.Fatalf("previous hung hook status when next hook started = %q; want dead", strings.TrimSpace(string(got)))
	}
}

func TestOpenCodeToolReachFailsClosedUnderNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
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

	hook := filepath.Join(dir, "hook.sh")
	// A reach into /repoB fails; an edit in /repoA is checked too, and passes.
	hookSrc := "#!/bin/sh\nif [ \"$1\" = \"opencode-tool-reach\" ]; then case \"$(cat)\" in *repoB*) exit 1;; esac; fi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := renderPlugin(opencodeConfig{
		Version:            1,
		Endpoint:           srv.URL,
		Signals:            []string{"traces"},
		IncludePrompts:     true,
		IncludeToolContent: true,
		HookCommand:        []string{hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(dir, "terma.mjs")
	if err := os.WriteFile(pluginPath, src, 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const hooks = await mod.default.server({ directory: "/repoA", worktree: "/repoA" })
` + opencodeDriverHelpers + `
await hooks.event(created("ses_fail"))
await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_fail", callID: "c1" }, { args: { filePath: "/repoB/secret.go" } })
await hooks.event(toolDone("ses_fail", "c1", "read", { filePath: "/repoB/secret.go" }, "SECRET"))
await hooks.event(assistantDone("ses_fail", "m1", "m"))
// Separate clean session still exports.
await hooks.event(created("ses_ok"))
await hooks["tool.execute.before"]({ tool: "edit", sessionID: "ses_ok", callID: "c2" }, { args: { filePath: "/repoA/a.go" } })
await hooks.event(toolDone("ses_ok", "c2", "edit", { filePath: "/repoA/a.go" }, "ok"))
await hooks.dispose()
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, pluginPath).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawEdit bool
	for _, name := range spanNames {
		if name == "execute_tool read" || name == "chat m" {
			t.Fatalf("failed opencode-tool-reach leaked span %q (all=%v)", name, spanNames)
		}
		if name == "execute_tool edit" {
			sawEdit = true
		}
	}
	if !sawEdit {
		t.Fatalf("expected execute_tool edit span for clean session; got %v", spanNames)
	}
}

func TestOpenCodeChildTaintSuppressesParentExportsUnderNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
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

	hook := filepath.Join(dir, "hook.sh")
	// A reach into /repoB fails; an edit in /repoA is checked too, and passes.
	hookSrc := "#!/bin/sh\nif [ \"$1\" = \"opencode-tool-reach\" ]; then case \"$(cat)\" in *repoB*) exit 1;; esac; fi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := renderPlugin(opencodeConfig{
		Version:            1,
		Endpoint:           srv.URL,
		Signals:            []string{"traces"},
		IncludePrompts:     true,
		IncludeToolContent: true,
		HookCommand:        []string{hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(dir, "terma.mjs")
	if err := os.WriteFile(pluginPath, src, 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const hooks = await mod.default.server({ directory: "/repoA", worktree: "/repoA" })
` + opencodeDriverHelpers + `
await hooks.event(created("ses_parent"))
await hooks["tool.execute.before"]({ tool: "task", sessionID: "ses_parent", callID: "t1" }, { args: { prompt: "inspect" } })
await hooks.event(created("ses_child", "ses_parent"))
await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_child", callID: "c1" }, { args: { filePath: "/repoB/secret.go" } })
await hooks.event(toolDone("ses_child", "c1", "read", { filePath: "/repoB/secret.go" }, "SECRET"))
// Parent task finishes and emits follow-up chat; sibling also runs: all must be suppressed.
await hooks.event(toolDone("ses_parent", "t1", "task", { prompt: "inspect" }, "SECRET SUMMARY"))
await hooks.event(assistantDone("ses_parent", "mp1", "parent-m"))
await hooks.event(created("ses_sibling", "ses_parent"))
await hooks.event(toolDone("ses_sibling", "s1", "edit", { filePath: "/repoA/s.go" }, "ok"))
// Unrelated session still exports.
await hooks.event(created("ses_other"))
await hooks.event(toolDone("ses_other", "o1", "write", { filePath: "/repoA/ok.go" }, "ok"))
await hooks.dispose()
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, pluginPath).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawOther bool
	for _, name := range spanNames {
		switch name {
		case "execute_tool read", "execute_tool task", "chat parent-m", "execute_tool edit":
			t.Fatalf("child reach failure leaked parent/child/sibling span %q (all=%v)", name, spanNames)
		case "execute_tool write":
			sawOther = true
		}
	}
	if !sawOther {
		t.Fatalf("expected execute_tool write span for unrelated session; got %v", spanNames)
	}
}

// opencodeDriverHelpers builds the OpenCode 1 events the drivers above feed the plugin: a
// session's record, a tool call's part once it ended, and a completed model call.
const opencodeDriverHelpers = `
const now = Date.now()
const created = (id, parentID) => ({ event: { type: "session.created", properties: { info: { id, parentID, directory: "/repoA", version: "1.19.6", title: "New session - 2026-01-01T00:00:00.000Z", time: { created: now - 1000 } } } } })
const toolDone = (sessionID, callID, tool, input, output) => ({ event: { type: "message.part.updated", properties: { part: { id: "prt_" + callID, sessionID, messageID: "msg_" + sessionID, callID, type: "tool", tool, state: { status: "completed", input, output, title: tool, metadata: {}, time: { start: now - 20, end: now - 10 } } } } } })
const assistantDone = (sessionID, id, modelID) => ({ event: { type: "message.updated", properties: { info: { id, sessionID, role: "assistant", parentID: "msg_user", modelID, time: { created: now - 10, completed: now } } } } })
`
