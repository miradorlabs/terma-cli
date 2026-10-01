package hookmgr

import (
	"testing"
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
}
