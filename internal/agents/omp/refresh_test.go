package omp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOmpRefreshMachineUpdatesInstalledExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envAgentDir, dir)
	path := filepath.Join(dir, extensionPath)

	// Absent file: no-op.
	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}

	// Inert raw template: no-op.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(extensionTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(inert) = (%v, %v); want (false, nil)", changed, err)
	}

	// Outdated installed extension: rewritten with current template, config preserved.
	old := `// Terma old extension
const CONFIG: TermaConfig | null = {"version":1,"hookCommand":["/custom/terma","hook"],"relay":"http://127.0.0.1:43180"} /* terma:config */
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
	for _, want := range []string{`"/custom/terma"`, `"http://127.0.0.1:43180"`, `"omp-file-edit"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed extension missing %s", want)
		}
	}
	// Second refresh is a no-op.
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}
