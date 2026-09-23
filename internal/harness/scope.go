package harness

import (
	"fmt"
	"strings"
)

// Scope is where a connect writes.
//
// Global is the harness's user-level configuration, which applies to every session on
// this machine: it holds the destination, the credential, and the identity. Local is a
// repository's own project settings, which the harness applies over the global file
// inside that repository: it holds only what to ship — which signals, and whether
// prompt and tool content go with them — so a committed project file never carries an
// endpoint or a key, and a repository with a local layer but no global connect exports
// nothing at all.
//
// Not to be confused with the Conflict.Scope strings (ScopeUserSettings and friends),
// which name where an *existing* setting was found.
type Scope string

// The two layers a connect can write. Global is the user's own settings, on this
// machine, for every repository; local is one repository's committed policy.
const (
	ScopeGlobal Scope = "global"
	ScopeLocal  Scope = "local"
)

// ParseScope reads a --scope value. Empty is global, the default a bare connect has
// always had.
func ParseScope(raw string) (Scope, error) {
	switch s := Scope(strings.ToLower(strings.TrimSpace(raw))); s {
	case "":
		return ScopeGlobal, nil
	case ScopeGlobal, ScopeLocal:
		return s, nil
	default:
		return "", fmt.Errorf("unknown scope %q (want global or local)", raw)
	}
}

// Scoped is a harness that can also be configured at repository scope. Codex is not:
// it reads one config file under CODEX_HOME and nothing from the working tree.
type Scoped interface {
	Harness
	// Local returns this harness bound to the repository at root. ConfigPath, Status,
	// ConflictsWith, Connect and Disconnect then act on the project settings file, and
	// what Connect writes is reduced to the keys a local layer may carry.
	Local(root string) Harness
	// Scope reports which layer this value acts on.
	Scope() Scope
}

// ScopeOf reports the layer h acts on; a harness without the notion is global.
func ScopeOf(h Harness) Scope {
	if s, ok := h.(Scoped); ok {
		return s.Scope()
	}
	return ScopeGlobal
}

// Reach is which repositories a *global* connect exports from. It is a different
// question from Scope, which is where the file goes: a global connect always writes the
// user's own settings, and Reach decides whether that file switches the exporters on for
// every session or leaves them off for repositories to turn on themselves.
//
// ReachRepos is the narrow half of the same layering `--scope local` uses. The global
// file keeps what only it can hold — the endpoint, the credential, the identity, and the
// harness's master telemetry switch — and writes every exporter off. A repository's
// committed policy then switches the signals it wants back on, and a repository without
// one sends nothing. So a work laptop that also holds personal projects reports the work
// and stays quiet elsewhere, without the developer maintaining a list.
//
// Nothing records the choice: a connect that points somewhere, holds a key and exports
// no signal *is* ReachRepos, because a repository policy is the only thing that can make
// it send. Status and doctor read it back that way rather than from a stored intent.
type Reach string

// The two reaches of a global connect. Everywhere exports from every session on the
// machine; repos leaves every exporter off so that a repository's policy decides.
const (
	ReachEverywhere Reach = "everywhere"
	ReachRepos      Reach = "repos"
)

// ParseReach reads an --exports value. Empty is everywhere, which is what a connect has
// always done.
func ParseReach(raw string) (Reach, error) {
	switch r := Reach(strings.ToLower(strings.TrimSpace(raw))); r {
	case "":
		return ReachEverywhere, nil
	case ReachEverywhere, ReachRepos:
		return r, nil
	default:
		return "", fmt.Errorf("unknown --exports value %q (want everywhere or repos)", raw)
	}
}
