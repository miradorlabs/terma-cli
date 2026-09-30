package dsh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plugin is private (it holds the relay's token), carries its configuration, and is
// inserted into dsh's home patch once, after whatever the developer's patch holds.
func TestWritePluginInsertsItselfOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	patch := filepath.Join(home, "cordis.patch.yml")
	if err := os.WriteFile(patch, []byte("- insert:\n    - id: mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := pluginConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": "Bearer tok"}, HookCommand: []string{"/x/terma", "hook"}}
	path, err := writePlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"endpoint":"http://127.0.0.1:43180"`, `"hookCommand":["/x/terma","hook"]`, `"version":1`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("plugin lacks %s", want)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("plugin is %v: it holds the relay's token", info.Mode().Perm())
	}
	if _, err := writePlugin(cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(patch)
	if !strings.HasPrefix(string(got), "- insert:\n    - id: mine\n") || strings.Count(string(got), "id: terma") != 1 {
		t.Fatalf("patch:\n%s", got)
	}
	if !strings.Contains(dshPluginTemplate, dshConfigMarker) {
		t.Fatal("the raw template must stay inert, with its configuration line")
	}
}
