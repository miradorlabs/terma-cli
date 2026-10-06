package pifamily

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent and lifecycle switch reach the rendered extension, the file is private, and
// the raw template stays inert.
func TestWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "extensions", "terma.ts")
	cfg := Config{Agent: "omp", Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": "Bearer tok"}, HookCommand: []string{"/x/terma", "hook"}}
	if _, err := Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`"agent":"omp"`, `"lifecycle":false`, `"hookCommand":["/x/terma","hook"]`} {
		if !strings.Contains(string(data), w) {
			t.Errorf("extension lacks %s", w)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("extension is %v: it holds the relay's token", info.Mode().Perm())
	}
	if !strings.Contains(template, configMarker) {
		t.Fatal("the raw template must stay inert, with its configuration line")
	}
}
