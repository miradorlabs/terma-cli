package hookmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// Global mode's machine-wide hooks: every agent's user-level file gets terma's entries
// by absolute path with --user, beside the developer's own; a second setup changes
// nothing, a moved terma is replaced in place, and removal leaves the developer's own.
func TestUserHooksInstallIdempotentlyAndRemoveCleanly(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		plan func(dir string, cmd func(string) string, install bool) (Plan, error)
		mine string
	}{
		{"claude", "settings.json", PlanClaudeUserHooks, `{"env":{"A":"1"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`},
		{"codex", "hooks.json", PlanCodexUserHooks, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`},
		{"cursor", "hooks.json", PlanCursorUserHooks, `{"version":1,"hooks":{"stop":[{"command":"say done"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.mine), 0o600); err != nil {
				t.Fatal(err)
			}
			apply := func(cmd func(string) string, install bool) Plan {
				t.Helper()
				p, err := tc.plan(dir, cmd, install)
				if err != nil {
					t.Fatal(err)
				}
				if err := Apply(dir, p); err != nil {
					t.Fatal(err)
				}
				return p
			}
			first := UserHookCommand("/opt/it's terma/bin/terma")
			apply(first, true)
			data, _ := os.ReadFile(path)
			if !strings.Contains(string(data), `hook --user`) || !strings.Contains(string(data), "say done") {
				t.Fatalf("after install:\n%s", data)
			}
			if p := apply(first, true); !p.Empty() {
				t.Fatalf("a second install changed %v", p.Changes)
			}
			moved := UserHookCommand("/home/dev/.local/bin/terma")
			apply(moved, true)
			data, _ = os.ReadFile(path)
			if strings.Contains(string(data), "/opt/it") || strings.Count(string(data), "hook --user") != strings.Count(string(data), "/home/dev/.local/bin/terma' hook") {
				t.Fatalf("a moved terma was not replaced in place:\n%s", data)
			}
			apply(moved, false)
			data, _ = os.ReadFile(path)
			if strings.Contains(string(data), "hook --user") || !strings.Contains(string(data), "say done") {
				t.Fatalf("after removal:\n%s", data)
			}
		})
	}
}

func TestUserHookCommandShape(t *testing.T) {
	cmd := UserHookCommand("/Users/dev/.local/bin/terma")("session-start")
	if cmd != `[ -x '/Users/dev/.local/bin/terma' ] && '/Users/dev/.local/bin/terma' hook --user session-start || true` {
		t.Fatalf("command = %s", cmd)
	}
	if !ownedHookCommand(cmd) || !ownedHookCommand(UserHookCommand("/a/it's/terma")("codex-stop")) {
		t.Fatal("a machine-wide entry is not recognized as terma's")
	}
	if ownedHookCommand(`[ -x '/bin/x' ] && '/bin/x' hook --user a; rm -rf / || true`) {
		t.Fatal("a command that only resembles terma's was taken for one")
	}
}

// The managed files an organization deploys hold every one of terma's hooks, each
// calling terma through $HOME, and parse as the agents read them.
func TestManagedConfig(t *testing.T) {
	cmd := ManagedHookCommand("$HOME/.local/bin/terma")
	if got := cmd("codex-stop"); got != `[ -x "$HOME/.local/bin/terma" ] && "$HOME/.local/bin/terma" hook --user codex-stop || true` || !ownedHookCommand(got) {
		t.Fatalf("managed command %q", got)
	}
	claude, err := ClaudeManagedSettings(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(claude, &settings); err != nil || len(settings.Hooks) != len(ClaudeHooks) {
		t.Fatalf("managed settings: %v, %d events\n%s", err, len(settings.Hooks), claude)
	}
	var req struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `toml:"type"`
				Command string `toml:"command"`
				Timeout int    `toml:"timeout"`
				Async   bool   `toml:"async"`
			} `toml:"hooks"`
		} `toml:"hooks"`
	}
	text := CodexManagedRequirements(cmd)
	if err := toml.Unmarshal([]byte(text), &req); err != nil {
		t.Fatalf("requirements.toml does not parse: %v\n%s", err, text)
	}
	for _, h := range CodexHooks {
		groups := req.Hooks[h.Event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", h.Event, groups)
		}
		got := groups[0].Hooks[0]
		if got.Type != "command" || got.Command != cmd(HookEventOf(h.Command)) || got.Timeout != h.Timeout || got.Async != h.Async {
			t.Errorf("%s: %+v", h.Event, got)
		}
	}
}
