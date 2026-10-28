package pifamily

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// The served model and the reasoning count are read under Pi's names and omp's, a reported 0
// stays 0, and a count neither reports stays absent rather than 0.
func TestExtensionReadsPiAndOmpResponses(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	// TypeScript runs under Node's type stripping, from 22.6; an older Node skips, a newer one
	// that cannot run the driver fails.
	version, err := exec.Command(node, "-p", `process.versions.node.split(".").map(Number).reduce((a, n, i) => i < 2 ? a * 1000 + n : a, 0)`).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	if v, _ := strconv.Atoi(strings.TrimSpace(string(version))); v < 22006 {
		t.Skipf("node %s strips no types", strings.TrimSpace(string(version)))
	}
	text, err := Render(Config{Agent: "omp", Endpoint: "http://127.0.0.1:1", HookCommand: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "terma.ts")
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
	out, err := exec.Command(node, "--experimental-strip-types", "--no-warnings", driver, path).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	got := map[string][2]string{}
	for line := range strings.Lines(string(out)) {
		var s struct {
			Attrs map[string]string `json:"attrs"`
		}
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		reasoning, ok := s.Attrs["gen_ai.usage.reasoning.output_tokens"]
		if !ok {
			reasoning = "absent"
		}
		got[s.Attrs["gen_ai.response.id"]] = [2]string{s.Attrs["gen_ai.response.model"], reasoning}
	}
	want := map[string][2]string{"r-pi": {"pi-served", "4"}, "r-omp": {"omp-served", "0"}, "r-none": {"requested", "absent"}}
	if !maps.Equal(got, want) {
		t.Fatalf("served model and reasoning count by response: got %v, want %v", got, want)
	}
}
