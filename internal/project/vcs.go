package project

import (
	"os"
	"path/filepath"
	"strings"
)

// UnsupportedVCS recognizes metadata directories in the nearest non-Git
// repository enclosing dir. This is only a warning heuristic: it never executes
// another VCS or changes workspace root selection.
func UnsupportedVCS(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for {
		var names []string
		for _, vcs := range []struct{ marker, name string }{
			{".hg", "Mercurial (hg)"},
			{"_darcs", "Darcs"},
			{".svn", "Subversion (svn)"},
			{"CVS", "CVS"},
			{".bzr", "Bazaar (bzr)"},
		} {
			if info, err := os.Stat(filepath.Join(dir, vcs.marker)); err == nil && info.IsDir() {
				names = append(names, vcs.name)
			}
		}
		if len(names) > 0 {
			return strings.Join(names, ", ")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
