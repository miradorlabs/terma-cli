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
