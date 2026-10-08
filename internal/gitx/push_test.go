package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// The remote-tracking branch shows a push only when the push went where the fetch comes
// from: one repository, however each URL spells it.
func TestTrackingRefFSFollowsThePushURL(t *testing.T) {
	for _, tc := range []struct {
		what, pushURL string
		ok            bool
	}{
		{what: "no push URL", ok: true},
		{what: "the same repository over SSH", pushURL: "git@github.com:acme/r.git", ok: true},
		{what: "another repository", pushURL: "git@github.com:acme/fork.git"},
		{what: "a local path", pushURL: "/srv/r.git"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			gitDir := t.TempDir()
			config := "[remote \"origin\"]\n\turl = https://github.com/acme/r.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
			if tc.pushURL != "" {
				config += "\tpushurl = " + tc.pushURL + "\n"
			}
			if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			ref, ok := TrackingRefFS(gitDir, "origin", "refs/heads/main")
			if ok != tc.ok || (ok && ref != "refs/remotes/origin/main") {
				t.Errorf("got %q, %v; want ok %v", ref, ok, tc.ok)
			}
		})
	}
}
