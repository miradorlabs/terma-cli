package hermes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The spliced configuration must come out of Python exactly as it went in: the token,
// the command, and any character a path or a header can hold.
func TestRenderHermesPluginIsValidPython(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	cfg := pluginConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": `Bearer t0k"en\é`},
		IncludePrompts: true, HookCommand: []string{"/Users/x y/.local/bin/terma", "hook"}}
	text, err := renderPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-c", "import sys; sys.path.insert(0, sys.argv[1]); import p, json; print(json.dumps(p.CONFIG, sort_keys=True))", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	for _, want := range []string{`"Bearer t0k\"en\\\u00e9"`, `"/Users/x y/.local/bin/terma"`, `"includePrompts": true`, `"includeToolContent": false`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("CONFIG lacks %s:\n%s", want, out)
		}
	}
	// The raw template is inert.
	if err := os.WriteFile(filepath.Join(dir, "raw.py"), []byte(hermesPluginTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(python, "-c", "import sys; sys.path.insert(0, sys.argv[1]); import raw; assert raw.CONFIG is None", dir).CombinedOutput(); err != nil {
		t.Fatalf("the raw template is not inert: %v\n%s", err, out)
	}
}
