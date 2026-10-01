package boundary

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docPath is a repository path standing on its own in prose.
var docPath = regexp.MustCompile(`(?:^|[^\w/.-])((?:internal|cmd|docs|scripts|live|npm)/[\w./-]*[\w/])`)

// goSymbol is a path that ends in a Go identifier (internal/selfupdate.AssetName).
var goSymbol = regexp.MustCompile(`^(.*)\.[A-Z]\w*$`)

// TestDocsNamePathsThatExist requires every repository path the README, CLAUDE.md,
// SECURITY.md and docs/ name to exist.
func TestDocsNamePathsThatExist(t *testing.T) {
	root := repoRoot(t)
	docs := []string{"README.md", "CLAUDE.md", "SECURITY.md"}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "compat" {
			return filepath.SkipDir // generated history: a record, not advice
		}
		if strings.HasSuffix(path, ".md") {
			rel, _ := filepath.Rel(root, path)
			docs = append(docs, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		data, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range docPath.FindAllStringSubmatch(line, -1) {
				path := strings.TrimRight(m[1], "./")
				if strings.Contains(path, "/bin/") || strings.HasSuffix(path, "/bin") {
					continue // a build's output
				}
				if exists(root, path) {
					continue
				}
				if s := goSymbol.FindStringSubmatch(path); s != nil && exists(root, s[1]) {
					continue
				}
				t.Errorf("%s:%d names %s, which does not exist", doc, i+1, path)
			}
		}
	}
}

func exists(root, path string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	return err == nil
}
