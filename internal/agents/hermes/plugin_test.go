package hermes

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The spliced configuration must come out of Python exactly as it went in: the token,
// the command, and any character a path or a header can hold.
func TestRenderHermesPluginIsValidPython(t *testing.T) {
	t.Parallel()
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

// A failed call is a chat span of its own, with its failure's category (Hermes's failover
// reason, else the exception's class name) and status code; the error's message and the
// request beside it never leave.
func TestPluginSpansAFailedCall(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	text, err := renderPlugin(pluginConfig{Endpoint: "http://127.0.0.1:1", HookCommand: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "termaplug.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read here, not only by python, so the test cache sees a changed driver.
	driver := filepath.Join(dir, "drive.py")
	src, err := os.ReadFile(filepath.Join("testdata", "drive.py"))
	if err == nil {
		err = os.WriteFile(driver, src, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-I", driver, dir).Output()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	type line struct {
		Name    string            `json:"name"`
		Error   bool              `json:"error"`
		Attrs   map[string]string `json:"attrs"`
		Private *bool             `json:"private"`
	}
	var spans []line
	for raw := range strings.Lines(string(out)) {
		var s line
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		spans = append(spans, s)
	}
	if len(spans) != 4 || spans[3].Private == nil {
		t.Fatalf("got %+v, want three spans and the private-text verdict", spans)
	}
	if *spans[3].Private {
		t.Fatal("the export holds the failure's message or request")
	}
	ok, failed := spans[0], spans[1]
	if ok.Error || ok.Attrs["gen_ai.response.model"] != "m-served" || ok.Attrs["error.type"] != "" {
		t.Errorf("successful call: %+v", ok)
	}
	if !failed.Error || failed.Name != "chat m" || failed.Attrs["error.type"] != "rate_limit" ||
		failed.Attrs["http.response.status_code"] != "429" || failed.Attrs["gen_ai.response.id"] != "t1:api:2" {
		t.Errorf("failed call: %+v", failed)
	}
	// Without a failover reason, the exception's class name is the category; no status, none sent.
	if unclassified := spans[2]; !unclassified.Error || unclassified.Attrs["error.type"] != "APIConnectionError" {
		t.Errorf("failed call without a reason: %+v", unclassified)
	} else if _, ok := unclassified.Attrs["http.response.status_code"]; ok {
		t.Errorf("failed call without a status sent one: %+v", unclassified)
	}
}
