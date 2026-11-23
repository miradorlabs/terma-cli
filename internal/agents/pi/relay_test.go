package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// Pi's exporter is terma's extension, written into Pi's agent directory, private to this
// user, pointed at the relay; the raw template stays inert.
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
	for _, w := range []string{`"endpoint":"http://127.0.0.1:43180"`, `"hookCommand":["/x/terma","hook"]`} {
		if !strings.Contains(string(data), w) {
			t.Errorf("extension lacks %s", w)
		}
	}
	if info, _ := os.Stat(want); info.Mode().Perm() != 0o600 {
		t.Errorf("extension is %v: it holds the relay's token", info.Mode().Perm())
	}
	if !strings.Contains(extensionTemplate, extensionConfigMarker) {
		t.Fatal("the raw template must stay inert, with its configuration line")
	}
}
