package dsh

import (
	"encoding/json"
	"os"
	"os/exec"
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

// Removing the plugin takes out only terma's insert, and the patch with it when nothing else was there.
func TestRemovePluginTakesOutItsInsert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	patch := filepath.Join(home, "cordis.patch.yml")
	cfg := pluginConfig{Endpoint: "http://127.0.0.1:43180"}
	for _, mine := range []string{"- insert:\n    - id: mine\n", ""} {
		if err := os.Remove(patch); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if mine != "" {
			if err := os.WriteFile(patch, []byte(mine), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		path, err := writePlugin(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := removePlugin(); err != nil || len(changed) != 2 {
			t.Fatalf("removePlugin = %v, %v", changed, err)
		}
		got, err := os.ReadFile(patch)
		if mine == "" && !os.IsNotExist(err) || mine != "" && string(got) != mine {
			t.Fatalf("patch after removal: %q, %v", got, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("plugin survived: %v", err)
		}
	}
}

// exportedSpan is one span a driver captured from a plugin's export.
type exportedSpan struct {
	Name  string            `json:"name"`
	Error bool              `json:"error"`
	Start int64             `json:"start"` // ms
	Attrs map[string]string `json:"attrs"`
}

// Each model call's span carries why it stopped and its reasoning count when the adapter
// reports one; an attempt that committed no message is its own span, charged to its own call
// of the agent loop (never an auxiliary one, nor one a message already settled), an error
// unless it was cancelled.
func TestPluginSpansEachModelCall(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	text, err := renderPlugin(pluginConfig{Endpoint: "http://127.0.0.1:1", HookCommand: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "terma.mjs")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read here, not only by node, so the test cache sees a changed driver.
	driver := filepath.Join(filepath.Dir(path), "drive.mjs")
	src, err := os.ReadFile(filepath.Join("testdata", "drive.mjs"))
	if err == nil {
		err = os.WriteFile(driver, src, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, driver, path).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var spans []exportedSpan
	for line := range strings.Lines(string(out)) {
		var s exportedSpan
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		spans = append(spans, s)
	}
	want := []struct {
		name, provider, finish, reasoning, input string
		error                                    bool
	}{
		{"chat deepseek-v4", "deepseek", "tool-calls", "4", "5", false},
		{"chat deepseek-v4", "deepseek", "error", "absent", "5", true},
		{"chat deepseek-v4", "deepseek", "aborted", "absent", "", false},
		{"chat deepseek-title", "deepseek", "stop", "absent", "1", false},
		{"chat deepseek-v4", "deepseek", "error", "absent", "", true},
		{"chat deepseek-v4", "deepseek", "stop", "absent", "", false},
		{"chat ", "", "error", "absent", "", true},
	}
	if len(spans) != len(want) {
		t.Fatalf("got %d spans, want %d: %+v", len(spans), len(want), spans)
	}
	for i, w := range want {
		s := spans[i]
		model := strings.TrimPrefix(w.name, "chat ")
		// An unreported count must stay absent, never collapse into a reported one.
		reasoning, ok := s.Attrs["gen_ai.usage.reasoning.output_tokens"]
		if !ok {
			reasoning = "absent"
		}
		if s.Name != w.name || s.Error != w.error || s.Attrs["gen_ai.response.finish_reasons"] != w.finish ||
			s.Attrs["gen_ai.request.model"] != model || s.Attrs["gen_ai.provider.name"] != w.provider ||
			reasoning != w.reasoning || s.Attrs["gen_ai.usage.input_tokens"] != w.input {
			t.Errorf("span %d = %+v, want %+v", i, s, w)
		}
	}
	// The attempt after a committed message starts where its step did (step/start at 1000).
	if last := spans[len(spans)-1]; last.Start != 1000 {
		t.Errorf("the attempt after a message starts at %d, want the step's 1000", last.Start)
	}
}
