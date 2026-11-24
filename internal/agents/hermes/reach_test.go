package hermes

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			if err := s.Bind(spec[2], spec[0], false); err != nil {
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
	if err := events["hermes-session-start"](ctx, env(`{"session_id":"`+parentID+`","cwd":"`+repoA+`"}`)); err != nil {
		t.Fatal(err)
	}
	parentRun, ok := runs.Locate(stateDir, parentID)
	if !ok || parentRun.Run == "" {
		t.Fatalf("hermes-session-start did not start run: %+v (ok=%v)", parentRun, ok)
	}
	if err := events["hermes-session-start"](ctx, env(`{"session_id":"`+childID+`","cwd":"`+repoA+`","parent_session_id":"`+parentID+`"}`)); err != nil {
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
		if err := events["hermes-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":"`+repoA+`"}`)); err != nil {
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
