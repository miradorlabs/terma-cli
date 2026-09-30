package hookmgr

import (
	"encoding/json"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

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
