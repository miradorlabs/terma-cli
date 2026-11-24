package opencode

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

	parentID := "ses_parent_oc"
	childID := "ses_child_oc"
	if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+parentID+`","cwd":"`+repoA+`"}`)); err != nil {
		t.Fatal(err)
	}
	parentRun, ok := runs.Locate(stateDir, parentID)
	if !ok || parentRun.Run == "" {
		t.Fatalf("opencode-session-start did not start run: %+v (ok=%v)", parentRun, ok)
	}
	if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+childID+`","cwd":"`+repoA+`","parent_session_id":"`+parentID+`"}`)); err != nil {
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
		if err := events["opencode-session-start"](ctx, env(`{"session_id":"`+sid+`","cwd":"`+repoA+`"}`)); err != nil {
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

func TestOpenCodePluginToolReachContractUnderNode(t *testing.T) {
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
const hooks = await mod.TermaPlugin({ directory: "/repoA", worktree: "/repoA" })
await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_oc", directory: "/repoA" } } } })
await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_oc", callID: "c1" }, { args: { filePath: "/repoB/secret.go" } })
await hooks["tool.execute.after"]({ tool: "read", sessionID: "ses_oc", callID: "c1", args: { filePath: "/repoB/secret.go" } }, { title: "read", output: "ok" })
await hooks["tool.execute.before"]({ tool: "grep", sessionID: "ses_oc", callID: "c2" }, { args: { path: "/repoB", include: "*.go" } })
await hooks["tool.execute.before"]({ tool: "glob", sessionID: "ses_oc", callID: "c3" }, { args: { pattern: "../repoB/**/*.go" } })
await hooks["tool.execute.before"]({ tool: "list", sessionID: "ses_oc", callID: "c4" }, { args: { path: "/repoB" } })
await hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_oc", callID: "c5" }, { args: { command: "cat ../repoB/a", workdir: "/repoB" } })
await hooks["tool.execute.before"]({ tool: "edit", sessionID: "ses_oc", callID: "c6" }, { args: { filePath: "/repoA/x.go" } })
await hooks["tool.execute.after"]({ tool: "edit", sessionID: "ses_oc", callID: "c6", args: { filePath: "/repoA/x.go" } }, { title: "edit", output: "ok" })
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
		if len(byTool) == 5 || time.Now().After(deadline) {
			break
		}
	}
	if reaches != 5 {
		t.Fatalf("opencode-tool-reach calls = %d (%v); want 5 (deduped across before/after, excluding edit)", reaches, byTool)
	}
	if _, ok := byTool["edit"]; ok {
		t.Fatalf("edit unexpectedly emitted opencode-tool-reach: %v", byTool["edit"])
	}
	if byTool["bash"]["command"] != "cat ../repoB/a" || byTool["bash"]["dir"] != "/repoB" {
		t.Fatalf("bash reach = %v", byTool["bash"])
	}
}
