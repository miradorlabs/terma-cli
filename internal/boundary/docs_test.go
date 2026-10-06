package boundary

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// docPath is a repository path standing on its own in prose.
var docPath = regexp.MustCompile(`(?:^|[^\w/.-])((?:internal|cmd|docs|scripts|test|npm)/[\w./-]*[\w/])`)

// goSymbol is a path that ends in a Go identifier (internal/selfupdate.AssetName).
var goSymbol = regexp.MustCompile(`^(.*)\.[A-Z]\w*$`)

// TestDocsNamePathsThatExist requires every repository path the README, AGENTS.md
// and docs/ name to exist.
func TestDocsNamePathsThatExist(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	docs := []string{"README.md", "AGENTS.md"}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
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

// docSymbol is a Go symbol in prose: `pkg.Name`, or `pkg.Type.Method`.
var docSymbol = regexp.MustCompile("`([a-z][a-z0-9]*)\\.([A-Z]\\w*)(?:\\.(\\w+))?")

// TestDocsNameSymbolsThatExist requires every `pkg.Name` the docs cite, for a package of
// this module, to be declared there: renames and moves left the docs naming code that
// was gone.
func TestDocsNameSymbolsThatExist(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	declared := map[string]map[string]bool{} // package name -> declared identifiers
	for _, p := range listPackages(t) {
		name := filepath.Base(p.ImportPath)
		if declared[name] == nil {
			declared[name] = map[string]bool{}
		}
		for _, f := range slices.Concat(p.GoFiles, p.TestGoFiles) {
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(p.Dir, f), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range file.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					declared[name][d.Name.Name] = true
				case *ast.GenDecl:
					for _, s := range d.Specs {
						switch s := s.(type) {
						case *ast.TypeSpec:
							declared[name][s.Name.Name] = true
						case *ast.ValueSpec:
							for _, n := range s.Names {
								declared[name][n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	docs := []string{"README.md", "AGENTS.md"}
	matches, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	for _, m := range matches {
		rel, _ := filepath.Rel(root, m)
		docs = append(docs, rel)
	}
	for _, doc := range docs {
		data, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range docSymbol.FindAllStringSubmatch(line, -1) {
				syms, ours := declared[m[1]]
				if !ours || syms[m[2]] {
					continue
				}
				t.Errorf("%s:%d names %s.%s, which package %s does not declare", doc, i+1, m[1], m[2], m[1])
			}
		}
	}
}
