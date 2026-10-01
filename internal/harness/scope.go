package harness

import (
	"fmt"
	"strings"
)

// Scope is where a connect writes: global holds the endpoint and credential, local only a
// repository's committed policy of what to ship.
type Scope string

// The two layers a connect can write.
const (
	ScopeGlobal Scope = "global"
	ScopeLocal  Scope = "local"
)

// ParseScope reads a --scope value; empty is global.
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

// Reach is which repositories a global connect exports from. Nothing stores it: connected
// and exporting no signal is ReachRepos, since only a repository policy can make it send.
type Reach string

// The two reaches of a global connect; repos writes every exporter off for repositories to switch on.
const (
	ReachEverywhere Reach = "everywhere"
	ReachRepos      Reach = "repos"
)

// ParseReach reads an --exports value; empty is everywhere.
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
