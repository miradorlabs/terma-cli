package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiRefreshMachineUpdatesInstalledExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	path := filepath.Join(dir, "extensions", "terma.ts")

	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `// Terma old pi extension
const CONFIG: TermaConfig | null = {"version":1,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer tok"},"includePrompts":true,"includeToolContent":true,"hookCommand":["/custom/terma","hook"]} /* terma:config */
export default function () {}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || !changed {
		t.Fatalf("RefreshMachine(outdated) = (%v, %v); want (true, nil)", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"/custom/terma"`, `"http://127.0.0.1:43180"`, `"-file-edit"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed extension missing %s", want)
		}
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}
