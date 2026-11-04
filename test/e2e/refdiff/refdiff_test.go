package refdiff

import (
	"fmt"
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

// A setting new at the top and in profiles is one change; one only a profile has is its own.
func TestConfigProfilesCollapse(t *testing.T) {
	opt := func(keys ...string) map[string]any {
		m := map[string]any{}
		for _, k := range keys {
			m[k] = map[string]any{"type": "string"}
		}
		return m
	}
	a := &Snapshot{Harness: "codex", Version: "0.1.0", Config: map[string]any{"source": "config.schema.json at rust-v0.1.0", "options": opt("model", "profiles.<name>.model")}}
	b := &Snapshot{Harness: "codex", Version: "0.1.1", Config: map[string]any{"source": "config.schema.json at rust-v0.1.1",
		"options": opt("model", "profiles.<name>.model", "web_search", "profiles.<name>.web_search", "profiles.<name>.only_here")}}
	var ids []string
	for _, c := range New(a, b, nil, time.Now()).Changes {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, " ") != "config.options.profiles.<name>.only_here config.options.web_search" {
		t.Errorf("changes = %v", ids)
	}
}

// Many variables added and removed are one change; a command only listed in one build has no
// options to compare.
func TestEnvGroupedAndUnhelped(t *testing.T) {
	vars := func(prefix string, n int) []any {
		out := []any{}
		for i := range n {
			out = append(out, fmt.Sprintf("%s_%02d", prefix, i))
		}
		return out
	}
	cmd := func(helped bool, opts map[string]any) map[string]any {
		return map[string]any{"commands": map[string]any{"claude agents": map[string]any{"helped": helped, "options": opts}}}
	}
	a := &Snapshot{Harness: "claude", Version: "1", Config: map[string]any{"env_vars": vars("OLD", 5)}, CLI: cmd(false, nil)}
	b := &Snapshot{Harness: "claude", Version: "2", Config: map[string]any{"env_vars": vars("NEW", 30)}, CLI: cmd(true, map[string]any{"--json": map[string]any{"names": []any{"--json"}}})}
	d := New(a, b, nil, time.Now())
	if len(d.Changes) != 1 || d.Changes[0].ID != "config.env_vars" || d.Changes[0].Title != "Environment variables: +30 −5" {
		t.Errorf("changes = %+v", d.Changes)
	}
}
