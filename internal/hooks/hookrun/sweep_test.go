package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Everything a hook kept past its retention goes in one sweep, whatever wrote it, while
// what is still in use stays.
func TestSweepAgesOutHookStateAndWorkspaces(t *testing.T) {
	t.Parallel()
	config := t.TempDir()
	now := time.Now()
	old := now.Add(-15 * 24 * time.Hour)

	write := func(rel string, at time.Time) {
		t.Helper()
		path := filepath.Join(config, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("cursors/old.json", old)
	write("cursors/old.json.lock", old)
	write("titles/orphan.json.lock", old)
	write("titles/"+".tmp-123", old)
	write("cursors/live.json", now)
	write("cursors/live.json.lock", old)

	// Manifests age by the updated_at they record, not by their files' times.
	store := func(name string, at time.Time) string {
		t.Helper()
		root := filepath.Join(config, project.WorkspacesDir, name)
		sess := session.Session{ID: "s-" + name, Tool: "agent", StartedAt: at, UpdatedAt: at}
		if err := session.Open(root).Touch(sess, []string{"a.go"}, at); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(root, "store.lock")
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
		return root
	}
	stale := store("stale", old)
	write(project.WorkspacesDir+"/stale/manifests/"+".tmp-1", old) // a crashed write's
	fresh := store("fresh", now)
	// A store a hook has just created holds only its lock, and stays.
	write(project.WorkspacesDir+"/new/store.lock", now)

	Sweep(config, now, []string{"cursors", "titles"})

	var left []string
	_ = filepath.WalkDir(config, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(config, path)
			left = append(left, filepath.ToSlash(rel))
		}
		return nil
	})
	want := []string{
		"cursors/live.json",
		"cursors/live.json.lock",
		project.WorkspacesDir + "/fresh/manifests/s-fresh.json",
		project.WorkspacesDir + "/fresh/store.lock",
		project.WorkspacesDir + "/new/store.lock",
	}
	if !slices.Equal(left, want) {
		t.Errorf("after the sweep:\n got %q\nwant %q", left, want)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale workspace's directory survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh workspace went: %v", err)
	}
}
