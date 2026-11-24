package dsh

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

func TestDshStartRunAndToolReachCrossOrgMixed(t *testing.T) {
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

	parentID := "session-dsh-parent"
	childID := "session-dsh-child"
	if err := events["dsh-session-start"](ctx, env(`{"session_id":"`+parentID+`","cwd":"`+repoA+`"}`)); err != nil {
		t.Fatal(err)
	}
	parentRun, ok := runs.Locate(stateDir, parentID)
	if !ok || parentRun.Run == "" {
		t.Fatalf("dsh-session-start did not start run: %+v (ok=%v)", parentRun, ok)
	}
	if err := events["dsh-session-start"](ctx, env(`{"session_id":"`+childID+`","cwd":"`+repoA+`","parent_session_id":"`+parentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if p, ok := runs.Locate(stateDir, childID); !ok || p.Run != parentRun.Run {
		t.Fatalf("subagent dsh-session-start run = %q (ok=%v); want parent run %q", p.Run, ok, parentRun.Run)
	}

	reaches := []map[string]any{
		{"tool": "read", "paths": []string{fileB}},
		{"tool": "grep", "paths": []string{repoB}, "globs": []string{"*.go"}},
		{"tool": "glob", "globs": []string{"../beta/**/*.go"}},
		{"tool": "bash", "command": "cat ../beta/secret.go"},
	}
	for i, payload := range reaches {
		sid := "session-dsh-reach-" + string(rune('1'+i))
		if err := events["dsh-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":"`+repoA+`"}`)); err != nil {
			t.Fatal(err)
		}
		payload["session_id"] = sid
		payload["cwd"] = repoA
		body, _ := json.Marshal(payload)
		if err := events["dsh-tool-reach"](ctx, env(string(body))); err != nil {
			t.Fatal(err)
		}
		if c, ok := claim.Read(stateDir, sid, now); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("%v: claim = %+v (ok=%v); want KindMixed", payload["tool"], c, ok)
		}
	}
}

func TestDshRefreshMachineUpdatesInstalledPlugin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_HOME", dir)
	path := filepath.Join(dir, "plugins", "terma-relay.mjs")

	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `// Terma old dsh plugin
const CONFIG = {"version":1,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer tok"},"includePrompts":true,"includeToolContent":true,"hookCommand":["/custom/terma","hook"]} /* terma:config */
export function apply() {}
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
	for _, want := range []string{`"/custom/terma"`, `"http://127.0.0.1:43180"`, `"dsh-tool-reach"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed plugin missing %s", want)
		}
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}

func TestDshPluginToolReachContractUnderNode(t *testing.T) {
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
	text, err := renderPlugin(pluginConfig{Endpoint: "http://127.0.0.1:43180", HookCommand: []string{hook}})
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(dir, "terma.mjs")
	if err := os.WriteFile(pluginPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const handlers = {}
mod.apply({ on: (name, fn) => { handlers[name] = fn } })
const sess = { id: "session-dsh-1" }
handlers["agent/created"]({ agent: { sessionId: "session-dsh-1", cwd: "/repoA" } })
handlers["session/event"](sess, { type: "tool/call", time: 1, data: { callId: "1", name: "read", arguments: JSON.stringify({ file_path: "/repoB/secret.go" }) } })
handlers["session/event"](sess, { type: "tool/call", time: 2, data: { callId: "2", name: "grep", arguments: { path: "/repoB", glob: "*.go" } } })
handlers["session/event"](sess, { type: "tool/call", time: 3, data: { callId: "3", name: "glob", arguments: { pattern: "../repoB/**/*.go" } } })
handlers["session/event"](sess, { type: "tool/call", time: 4, data: { callId: "4", name: "ls", arguments: { path: "/repoB" } } })
handlers["session/event"](sess, { type: "tool/call", time: 5, data: { callId: "5", name: "bash", arguments: { command: "cat ../repoB/a", cwd: "/repoB" } } })
handlers["session/event"](sess, { type: "tool/call", time: 6, data: { callId: "6", name: "edit", arguments: { file_path: "/repoA/a.go" } } })
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
			if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &c) == nil && c.Event == "dsh-tool-reach" {
				reaches++
				if tool, _ := c.Payload["tool"].(string); tool != "" {
					byTool[tool] = c.Payload
				}
			}
		}
		if len(byTool) == 5 || time.Now().After(deadline) {
			break
		}
	}
	if reaches != 5 {
		t.Fatalf("dsh-tool-reach calls = %d (%v); want 5", reaches, byTool)
	}
	if _, ok := byTool["edit"]; ok {
		t.Fatalf("edit unexpectedly emitted dsh-tool-reach")
	}
	if byTool["bash"]["command"] != "cat ../repoB/a" || byTool["bash"]["dir"] != "/repoB" {
		t.Fatalf("bash reach = %v", byTool["bash"])
	}
}
