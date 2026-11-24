//go:build unix

package hookrun

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSafeLocateRejectsForeignOwnedRepo(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	for _, sub := range []string{".git/objects", ".git/refs"} {
		if err := os.MkdirAll(filepath.Join(repo, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		switch k {
		case "HOME":
			return parent, true
		case "GIT_CONFIG_NOSYSTEM":
			return "1", true
		}
		return "", false
	}
	scOwn := newScan(repo, env, work{deadline: time.Now().Add(time.Minute)})
	if _, ok := scOwn.safeLocate(repo); !ok {
		t.Fatalf("safeLocate on self-owned repo = false, want true")
	}

	origUID := effectiveUID
	t.Cleanup(func() { effectiveUID = origUID })
	effectiveUID = func() int { return os.Geteuid() + 1 }

	scForeign := newScan(repo, env, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scForeign.safeLocate(repo); ok {
		t.Fatalf("safeLocate on foreign-owned repo = (%q, true), want false", got)
	}
	scForeignEnv := newScan(repo, func(k string) (string, bool) {
		switch k {
		case "GIT_DIR":
			return filepath.Join(repo, ".git"), true
		case "GIT_WORK_TREE":
			return repo, true
		}
		return env(k)
	}, work{deadline: time.Now().Add(time.Minute)})
	if got, ok := scForeignEnv.safeLocate(repo); ok {
		t.Fatalf("safeLocate with GIT_DIR/GIT_WORK_TREE on foreign-owned repo = (%q, true), want false", got)
	}
}
