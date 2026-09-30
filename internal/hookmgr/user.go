package hookmgr

import (
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
