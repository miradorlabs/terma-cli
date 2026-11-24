package boundary

import (
	"slices"
	"testing"
)

// TestTheRunLedgerIsSharedByHooksAndDelivery keeps internal/runs where both its writers,
// the hooks, and its readers, the relay and delivery, may import it: it reaches none of
// their packages, nor the network or the keychain, which the hooks may not.
func TestTheRunLedgerIsSharedByHooksAndDelivery(t *testing.T) {
	t.Parallel()
	banned := []string{"internal/relay", "internal/delivery", "internal/hooks", "internal/agents", "internal/account", "internal/cli"}
	found := false
	for _, p := range listPackages(t) {
		if !within(p.ImportPath, "internal/runs") {
			continue
		}
		found = true
		for _, dep := range p.Deps {
			if dep == "net/http" || slices.ContainsFunc(banned, func(b string) bool { return within(dep, b) }) {
				t.Errorf("%s reaches %s: the run ledger is shared by hooks, the relay and delivery", p.ImportPath, dep)
			}
		}
	}
	if !found {
		t.Fatal("no internal/runs package")
	}
}
