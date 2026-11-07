package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// copyTree copies every file under src to dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	for path, data := range treeBytes(t, src) {
		to := filepath.Join(dst, path)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A republish into a published tree (one pass over each observed field, files that hold their
// bytes already left as they are) writes what a full render (-full: every field walked, every
// file written) writes, a snapshot published before a machine-path rule among them.
func TestIncrementalRenderIsAFullOne(t *testing.T) {
	pub := t.TempDir()
	for i, v := range []string{"1.0.0", "1.0.1", "1.0.2"} {
		n := night{now: time.Date(2026, 11, 3+i, 5, 0, 0, 0, time.UTC), profiles: append(machineProfiles(v), filesRow(v, "linux/amd64", nil))}
		if _, err := publish(pub, n, options{}); err != nil {
			t.Fatal(err)
		}
	}
	// As published before a rule: a machine path, and a run id, in an observed value.
	path := filepath.Join(pub, "claude", "versions", "1.0.1.json")
	var s Snapshot
	if err := readJSON(path, &s); err != nil {
		t.Fatal(err)
	}
	s.CLI["launch"].(map[string]any)["env_values"].(map[string]any)["X"] = "/opt/homebrew/bin/node local_88d385d8-fbba-4414-a316-90943ceface5"
	if err := writeJSON(path, &s); err != nil {
		t.Fatal(err)
	}
	next := night{now: time.Date(2026, 11, 7, 5, 0, 0, 0, time.UTC), profiles: machineProfiles("1.0.3"),
		history: []HistoryEntry{{Harness: "claude", Version: "1.0.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"}}}
	incremental, full := t.TempDir(), t.TempDir()
	copyTree(t, pub, incremental)
	copyTree(t, pub, full)
	if _, err := publish(incremental, next, options{}); err != nil {
		t.Fatal(err)
	}
	fullRender = true
	_, err := publish(full, next, options{})
	fullRender = false
	if err != nil {
		t.Fatal(err)
	}
	a, b := treeBytes(t, incremental), treeBytes(t, full)
	if !reflect.DeepEqual(a, b) {
		for k := range b {
			if a[k] != b[k] {
				t.Errorf("%s differs from a full render", k)
			}
		}
		for k := range a {
			if _, ok := b[k]; !ok {
				t.Errorf("%s is not in a full render", k)
			}
		}
	}
	var again Snapshot
	if err := readJSON(filepath.Join(incremental, "claude", "versions", "1.0.1.json"), &again); err != nil {
		t.Fatal(err)
	}
	if got := again.CLI["launch"].(map[string]any)["env_values"].(map[string]any)["X"]; got != "node local_<uuid>" {
		t.Errorf("the republished value = %q", got)
	}
}

// A republish that changes nothing rewrites nothing.
func TestRepublishLeavesUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	n := night{now: time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{claudeProfile("1.0.0", "Be.", []string{"Bash"}, "", ""), claudeProfile("1.0.1", "Be!", []string{"Bash"}, "", "")}}
	if _, err := publish(dir, n, options{}); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for path := range treeBytes(t, dir) {
		if err := os.Chtimes(filepath.Join(dir, path), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := publish(dir, night{now: n.now}, options{}); err != nil {
		t.Fatal(err)
	}
	for path := range treeBytes(t, dir) {
		st, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if !st.ModTime().Equal(old) {
			t.Errorf("%s was rewritten", path)
		}
	}
}

// Which sections a snapshot has is what its JSON says, read from its fields.
func TestSectionPresentIsTheJSONs(t *testing.T) {
	for _, s := range []*Snapshot{
		{},
		{Model: map[string]any{}, Tools: []any{}, SideRequests: []any{}},
		{Model: map[string]any{"a": 1}, Tools: []any{1}, SideRequests: nil, Support: &refdiff.Support{}},
		{Filesystem: map[string]any{"linux/amd64": nil}, Binary: map[string]any{"x": nil}, CLI: map[string]any{}},
	} {
		tree := s.Tree()
		for _, sec := range sections {
			if got, want := sectionPresent(s, sec), tree[sec] != nil; got != want {
				t.Errorf("%+v: %s present %v, its JSON says %v", s, sec, got, want)
			}
		}
	}
}

// hasUUID finds a uuid wherever uuidPattern would, and more.
func TestHasUUID(t *testing.T) {
	for s, want := range map[string]bool{
		`"local_88d385d8-fbba-4414-a316-90943ceface5"`: true,
		`"\n4D2B7FD5-E813-4AE3-B573-50692BF90F1C"`:     true,
		`88d385d8-fbba-4414-a316-90943ceface`:          false,
		`<uuid>/<uuid>`:                                false,
		`2026-11-07-12-00`:                             false,
	} {
		if got := hasUUID([]byte(s)); got != want {
			t.Errorf("hasUUID(%s) = %v", s, got)
		}
		if uuidPattern.MatchString(s) && !hasUUID([]byte(s)) {
			t.Errorf("uuidPattern finds an id in %s that hasUUID does not", s)
		}
	}
}
