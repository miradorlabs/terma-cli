package boundary

import (
	"slices"
	"testing"
)

// offline are the hook runtime, the spool and the agents, none of which links net/http, even
// through what it imports: a hook appends to the spool and exits, and a detached
// `terma spool flush` sends it.
var offline = []string{"internal/hooks", "internal/spool", "internal/agents"}

// TestHooksNeverReachTheNetwork holds "hooks never touch the network" to the import graph.
func TestHooksNeverReachTheNetwork(t *testing.T) {
	t.Parallel()
	for _, p := range listPackages(t) {
		if !slices.ContainsFunc(offline, func(o string) bool { return within(p.ImportPath, o) }) {
			continue
		}
		if slices.Contains(p.Deps, "net/http") {
			t.Errorf("%s reaches net/http: hooks never touch the network; internal/delivery sends the spool", p.ImportPath)
		}
	}
}

// TestHooksNeverReadTheKeychain keeps the system keychain off the hook path: a keychain
// read runs a process on macOS, and prepare-commit-msg has 50 ms.
func TestHooksNeverReadTheKeychain(t *testing.T) {
	t.Parallel()
	const keychain = "github.com/miradorlabs/terma-cli/internal/account/secret"
	for _, p := range listPackages(t) {
		if !slices.ContainsFunc(offline, func(o string) bool { return within(p.ImportPath, o) }) {
			continue
		}
		if slices.Contains(p.Deps, keychain) {
			t.Errorf("%s reaches the keychain: hooks never read secrets; delivery and the relay do", p.ImportPath)
		}
	}
}
