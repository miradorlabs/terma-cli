package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Codex reads through the shell. On a migrated machine where Acme Platform collects
// github.com/acme/*, bound to Acme, and personal only github.com/dawson/*, a thread that
// reads Acme's checkout from personal's has crossed, though it wrote nothing and nothing is
// spooled for the read.
func TestRoutedCodexShellReadsReachingAcmeMakeTheRunMixed(t *testing.T) {
	const sid = "01a08bd5-0487-74b1-9d82-45e619c57aaa"
	for name, from := range map[string]func(t *testing.T, side string) string{
		"from personal's checkout": func(_ *testing.T, side string) string { return side },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			side := hookruntest.RepoAt(t, "github.com/dawson/side")
			api := filepath.Join(filepath.Dir(side), "acme-api")
			if err := os.Rename(hookruntest.RepoAt(t, "github.com/acme/api"), api); err != nil {
				t.Fatal(err)
			}
			hookruntest.WriteFile(t, api, "NOTES.md", "acme\n")
			st := hookruntest.RoutingState(t, []routing.Team{
				hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"),
				hookruntest.RoutedPersonal("github.com/dawson/*"),
			}, map[string]string{"github.com/acme": "acme"})
			stateDir := t.TempDir()
			sp, _ := spool.Open(t.TempDir())
			hook := func(handler func(context.Context, hookrun.Env) error, cwd string, payload map[string]any) {
				t.Helper()
				payload["session_id"], payload["cwd"] = sid, cwd
				b, _ := json.Marshal(payload)
				claimed := false
				env := hookrun.Env{StateDir: stateDir, ConfigDir: t.TempDir(), Now: time.Now(), Cwd: cwd, Spool: sp, Version: "test",
					Policy: config.NoPolicy("", ""), Routing: hookrun.NewRouting(hookruntest.RoutedProfile, st),
					Stdin: strings.NewReader(string(b)), OnClaim: func(string) { claimed = true }}
				if err := handler(context.Background(), env); err != nil {
					t.Fatal(err)
				}
				if !claimed {
					hookrun.ClaimFromPayload(context.Background(), env, hookrun.PayloadSession{ID: sid, Cwd: cwd}, codexTool)
				}
			}
			mixed := func() bool {
				t.Helper()
				_, ledger := runs.MixedAt(stateDir, sid, time.Time{})
				c, ok := claim.Read(stateDir, sid, time.Now())
				if placement := ok && c.Kind == claim.KindMixed; placement != ledger {
					t.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
				}
				return ledger
			}
			at := from(t, side)
			hook(sessionStart, at, map[string]any{"hook_event_name": "SessionStart", "model": "gpt-6"})
			hookruntest.Spooled(t, sp)
			hook(postToolUse, at, map[string]any{"hook_event_name": "PostToolUse", "tool_name": codexShellTool, "tool_use_id": "c1",
				"tool_input": map[string]any{"command": "cat " + filepath.Join(api, "NOTES.md") + " | head -n 5"}})
			if evs := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaFilesTouchedEvent); len(evs) != 0 {
				t.Fatalf("a read spooled file events %+v", evs)
			}
			if at != side {
				if mixed() {
					t.Fatal("Mixed before the run was anywhere an organization collects")
				}
				hook(userPromptSubmit, side, map[string]any{"hook_event_name": "UserPromptSubmit"})
			}
			if !mixed() {
				t.Fatal("not Mixed after reading Acme's checkout")
			}
		})
	}
}
