package adapter

import "github.com/miradorlabs/terma-cli/internal/hookmgr"

// UserHooks is an adapter that supports machine-wide hook installation.
type UserHooks interface {
	Adapter
	UserHooksPath() (string, error)
	UserHookSelections() []string
	PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error)
}

// ManagedHooks is an adapter whose machine-wide hooks can be deployed by IT.
type ManagedHooks interface {
	ManagedHookFiles(root string) []string
}

// UserHookAdapters returns the machine-wide hook capabilities in registry order.
func UserHookAdapters() []UserHooks {
	var out []UserHooks
	for _, a := range registry {
		if hooks, ok := a.(UserHooks); ok {
			out = append(out, hooks)
		}
	}
	return out
}

var (
	_ UserHooks    = claude{}
	_ UserHooks    = codex{}
	_ UserHooks    = cursor{}
	_ ManagedHooks = claude{}
	_ ManagedHooks = codex{}
)
