package daemon

import (
	"strings"
	"testing"
)

// Separate configuration directories must not share a service.
func TestRelayServiceNames(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	a, _ := serviceName()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	b, _ := serviceName()
	if a == b || !strings.HasPrefix(a, "ai.terma.relay.") {
		t.Fatalf("service names %q and %q must differ per config directory", a, b)
	}
}
