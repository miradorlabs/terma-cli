package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pi's extension and omp's are one template: the agent names every record and hook,
// and omp's reports no lifecycle (its committed hook file does).
func TestPiFamilyExtensions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv(ompConfigOverride, "")
	cfg := PiConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": "Bearer tok"}, HookCommand: []string{"/x/terma", "hook"}}
	pi, err := WritePiExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	omp, err := WriteOmpRelayExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]string{
		filepath.Join(home, ".pi", "agent", "extensions", "terma.ts"):        {`"agent":"pi"`, `"lifecycle":true`},
		filepath.Join(home, ".omp", "agent", "extensions", "terma-relay.ts"): {`"agent":"omp"`, `"lifecycle":false`},
	} {
		if path != pi && path != omp {
			t.Fatalf("written to %s and %s, want %s", pi, omp, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range append(want, `"hookCommand":["/x/terma","hook"]`) {
			if !strings.Contains(string(data), w) {
				t.Errorf("%s lacks %s", path, w)
			}
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v: it holds the relay's token", path, info.Mode().Perm())
		}
	}
	if !strings.Contains(piExtensionTemplate, piConfigMarker) {
		t.Fatal("the raw template must stay inert, with its configuration line")
	}
}
