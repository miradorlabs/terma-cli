package hookmgr

import (
	"strings"
)

// ManagedHookCommand is UserHookCommand for an organization's managed configuration, which
// serves every account: terma under $HOME, double-quoted so the shell expands it per user.
func ManagedHookCommand(terma string) func(event string) string {
	return userHookCommand(`"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`").Replace(terma) + `"`)
}
