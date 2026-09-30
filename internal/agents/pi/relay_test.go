package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// Pi's exporter is terma's extension, written into Pi's agent directory with the
// session lifecycle on.
func TestConfigureRelayWritesTheExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	res, err := Agent{}.ConfigureRelay(context.Background(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: []string{"/x/terma", "hook"}})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "extensions", "terma.ts")
	if len(res.Paths) != 1 || res.Paths[0] != want {
		t.Fatalf("paths %v, want %s", res.Paths, want)
	}
	data, _ := os.ReadFile(want)
	for _, w := range []string{`"agent":"pi"`, `"lifecycle":true`, `"hookCommand":["/x/terma","hook"]`} {
		if !strings.Contains(string(data), w) {
			t.Errorf("extension lacks %s", w)
		}
	}
}
