package repohooks

import (
	"path/filepath"
	"testing"
)

// On a migrated machine what asks for the hooks is routing, judged by the repository's git
// directory: SyncIf installs them only where it says yes, and asks about the repository the
// session's directory is in.
func TestSyncIfInstallsOnlyWhereAsked(t *testing.T) {
	for _, want := range []bool{true, false} {
		root, gitDir := scratch(t)
		git(t, root, "remote", "add", "origin", "git@github.com:acme/repo.git")
		var asked string
		if _, err := SyncIf(t.TempDir(), terma, func(g string) bool { asked = g; return want }, filepath.Join(root, ".")); err != nil {
			t.Fatalf("SyncIf: %v", err)
		}
		if got, _ := Installed(gitDir); got != want {
			t.Errorf("installed = %v, want %v", got, want)
		}
		if resolved(asked) != resolved(gitDir) {
			t.Errorf("asked about %q, want the repository's git directory %q", asked, gitDir)
		}
	}
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
