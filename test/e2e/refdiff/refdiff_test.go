package refdiff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnifiedDiff(t *testing.T) {
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\ntwo\nTHREE\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n"
	d, stats := unifiedDiff(a, b)
	want := "@@ -1,6 +1,6 @@\n one\n two\n-three\n+THREE\n four\n five\n six\n@@ -8,4 +8,5 @@\n eight\n nine\n ten\n+eleven\n \n"
	if d != want {
		t.Errorf("diff =\n%s\nwant\n%s", d, want)
	}
	if stats.AddedChars != 11 || stats.RemovedChars != 5 {
		t.Errorf("stats = %+v", stats)
	}
	long := strings.Repeat("A sentence here. ", 30)
	d, _ = unifiedDiff(long, strings.Replace(long, "A sentence here. A", "A sentence there. A", 1))
	if strings.Count(d, "\n-") != 1 || strings.Count(d, "\n+") != 1 {
		t.Errorf("a long line compared whole:\n%s", d)
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"2.1.9", "2.1.10"}, {"0.2.1-rc.5", "0.2.1"}, {"2026.9.21", "2026.9.24"}, {"0.159.0", "0.159.1"}} {
		if !VersionLess(c[0], c[1]) || VersionLess(c[1], c[0]) {
			t.Errorf("%s < %s not said", c[0], c[1])
		}
	}
}

// The documented judgement of a range reads every changelog in it, and none outside.
func TestCompareRange(t *testing.T) {
	snap := func(v string, tools ...string) *Snapshot {
		s := &Snapshot{Harness: "claude", Version: v}
		for _, name := range tools {
			s.Tools = append(s.Tools, map[string]any{"name": name})
		}
		return s
	}
	notes := map[string]ChangelogNote{
		"1.0.0": {Text: "Added Read"}, "1.0.1": {Text: "Added Monitor"}, "1.0.2": {Text: "Fixed a bug"}, "1.0.3": {Text: "Added Grep"}}
	d := CompareRange(snap("1.0.0", "Bash"), snap("1.0.2", "Bash", "Monitor", "Read"), notes, time.Now())
	var monitor, read *Change
	for i := range d.Changes {
		switch d.Changes[i].ID {
		case "tools.Monitor":
			monitor = &d.Changes[i]
		case "tools.Read":
			read = &d.Changes[i]
		}
	}
	if monitor == nil || !*monitor.Documented {
		t.Errorf("Monitor, added in 1.0.1's changelog: %+v", monitor)
	}
	if read == nil || *read.Documented {
		t.Errorf("Read, named only in 1.0.0's, before the range: %+v", read)
	}
	if d.From != "1.0.0" || d.To != "1.0.2" || d.Changelog == nil || strings.Contains(d.Changelog.Text, "Grep") {
		t.Errorf("diff %s..%s changelog %+v", d.From, d.To, d.Changelog)
	}
	if d := CompareRange(snap("1.0.0"), snap("1.0.0"), nil, time.Now()); d.Changelog != nil || len(d.Changes) != 0 {
		t.Errorf("a version against itself: %+v", d)
	}
}

// The browser's comparison builds.
func TestWasmBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the WebAssembly")
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "refdiff.wasm"), "./wasm")
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("GOOS=js GOARCH=wasm go build: %v\n%s", err, out)
	}
}

// Storage as a run left it: a database unread, a marker file, are not observed changes; a new
// kind with records is.
func TestStorageObserved(t *testing.T) {
	a := &Snapshot{Harness: "codex", Version: "0.1.0", Storage: map[string]any{
		"logs":         map[string]any{"format": "sqlite", "path": "<codex-home>/logs.db", "tables": map[string]any{"logs": []any{"id"}}},
		"last-cleanup": map[string]any{"format": "text", "path": "<claude-config>/.last-cleanup"}}}
	b := &Snapshot{Harness: "codex", Version: "0.1.1", Storage: map[string]any{
		"logs":       map[string]any{"format": "sqlite", "path": "<codex-home>/logs.db"},
		"transcript": map[string]any{"format": "jsonl", "path": "<codex-home>/sessions/x.jsonl", "record_types": map[string]any{"turn": []any{"id"}}}}}
	d := New(a, b, nil, time.Now())
	if len(d.Changes) != 1 || d.Changes[0].ID != "storage.transcript" || d.Changes[0].Op != "added" {
		t.Errorf("changes = %+v", d.Changes)
	}
}
