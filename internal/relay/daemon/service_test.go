package daemon

import (
	"strings"
	"testing"
)

// Separate configuration directories must not share a service.
func TestRelayServiceNames(t *testing.T) {
	t.Parallel()
	a, b := serviceName(t.TempDir()), serviceName(t.TempDir())
	if a == b || !strings.HasPrefix(a, "ai.terma.relay.") {
		t.Fatalf("service names %q and %q must differ per config directory", a, b)
	}
}
