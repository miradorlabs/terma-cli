package hookmgr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Global mode's hooks deployed by an organization as managed configuration (`terma
// setup --managed-config`): the same entries as the machine-wide ones, in the files an
// agent reads from the system and never asks a developer to trust — Claude Code's
// managed settings, Codex's requirements.toml. One file serves every account on the
// machine, so terma is named relative to $HOME, where the installer puts it.

// ManagedHookCommand is UserHookCommand for a managed file: terma at a path the shell
// expands per user ($HOME/...), double-quoted so it does.
func ManagedHookCommand(terma string) func(event string) string {
	q := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`").Replace(terma) + `"`
	return func(event string) string {
		return "[ -x " + q + " ] && " + q + " hook --user " + event + " || true"
	}
}

// ClaudeManagedSettings is a managed-settings.json holding terma's hooks.
func ClaudeManagedSettings(command func(event string) string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "terma-managed")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	plan, err := planClaude(dir, "managed-settings.json", command, true)
	if err != nil {
		return nil, err
	}
	if err := Apply(dir, plan); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "managed-settings.json"))
}

// CodexManagedRequirements is the [hooks] table of a Codex requirements.toml holding
// terma's hooks (codex-rs config: ManagedHooksRequirementsToml, the events flattened
// under it). Codex runs managed hooks without asking anyone to trust them.
func CodexManagedRequirements(command func(event string) string) string {
	var b strings.Builder
	b.WriteString("# terma: global mode's hooks for every Codex session on this machine.\n[hooks]\n")
	for _, h := range CodexHooks {
		fmt.Fprintf(&b, "\n[[hooks.%s]]\n\n[[hooks.%s.hooks]]\ntype = \"command\"\ncommand = %s\ntimeout = %d\n", h.Event, h.Event, tomlString(command(HookEventOf(h.Command))), h.Timeout)
		if h.Async {
			b.WriteString("async = true\n")
		}
	}
	return b.String()
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
