//go:build unix

package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallE2EUnsupportedVCS(t *testing.T) {
	for _, vcs := range []struct{ marker, name string }{
		{".hg", "Mercurial (hg)"},
		{"_darcs", "Darcs"},
		{".svn", "Subversion (svn)"},
		{"CVS", "CVS"},
		{".bzr", "Bazaar (bzr)"},
	} {
		t.Run(vcs.marker, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("checkout")
			s.write(root, vcs.marker+"/user-metadata", "preserve exactly\n")
			workspace := s.mkdir("checkout", "deep", "workspace")
			assertWarning := func(out string) {
				t.Helper()
				if !strings.Contains(out, "Warning: detected "+vcs.name) || !strings.Contains(out, "Terma only supports Git") {
					t.Fatalf("missing unsupported VCS warning:\n%s", out)
				}
			}
			assertWarning(s.install(workspace, "--dry-run"))
			requireAbsent(t, filepath.Join(workspace, ".terma"))
			assertWarning(s.install(workspace))
			readInstallFile(t, workspace, ".claude/settings.json")
			readInstallFile(t, workspace, ".terma/settings.json")
			requireAbsent(t, filepath.Join(workspace, ".terma", "hooks"))
			requireAbsent(t, filepath.Join(root, ".terma"))
			nested := s.mkdir("checkout", "deep", "workspace", "child")
			assertWarning(s.install(nested, "--no-hooks"))
			s.cli(nested, "uninstall", "--yes")
			if got := string(readInstallFile(t, root, vcs.marker+"/user-metadata")); got != "preserve exactly\n" {
				t.Fatalf("VCS metadata changed: %q", got)
			}
			requireAbsent(t, filepath.Join(root, ".git"))
		})
	}
}

func TestInstallE2EVCSWarningBoundaries(t *testing.T) {
	for _, kind := range []string{"ordinary_folder", "marker_is_file", "git_with_other_metadata", "git_inside_hg"} {
		t.Run(kind, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			switch kind {
			case "marker_is_file":
				s.write(root, "CVS", "ordinary user file")
			case "git_with_other_metadata":
				s.mkdir("workspace", ".hg")
				s.git(root, "init", "-q")
			case "git_inside_hg":
				s.mkdir("workspace", ".hg")
				root = s.mkdir("workspace", "git-project")
				s.git(root, "init", "-q")
			}
			if out := s.install(root, "--dry-run"); strings.Contains(out, "Warning: detected") {
				t.Fatalf("unexpected VCS warning:\n%s", out)
			}
		})
	}
}
