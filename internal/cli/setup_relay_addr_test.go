package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// --relay-addr records a loopback address for the relay and refuses anything else.
func TestMoveRelayTakesOnlyALoopbackAddress(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	for _, bad := range []string{"0.0.0.0:4319", "192.168.1.5:4319", "4319", "example.com:4319"} {
		if err := moveRelay(bad); err == nil {
			t.Errorf("moveRelay(%q) accepted a non-loopback address", bad)
		}
	}
	for _, good := range []string{"127.0.0.1:4320", "localhost:4321", "[::1]:4322"} {
		if err := moveRelay(good); err != nil {
			t.Fatalf("moveRelay(%q): %v", good, err)
		}
		dir, err := daemon.Dir()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, daemon.AddrFile))
		if err != nil || strings.TrimSpace(string(data)) != good {
			t.Fatalf("recorded %q, %v; want %q", data, err, good)
		}
		if got := daemon.Addr(dir); got != good {
			t.Fatalf("daemon.Addr = %q, want %q", got, good)
		}
	}
}
