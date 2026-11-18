package boundary

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// maxLines is where a source file has usually grown a second job.
const maxLines = 450

// longFiles hold one job that reads best whole.
var longFiles = map[string]string{
	"internal/session/session.go":   "the store: every writer under one lock, read together",
	"internal/agents/codex/toml.go": "the [otel] splice, verified before it is written, reviewed as one",
	"internal/semconv/semconv.go":   "rendered whole from the Weaver registry, never edited by hand",
}

// No source file outgrows its job: split one past maxLines by responsibility, or say here
// why it is one.
func TestNoSourceFileOutgrowsItsJob(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			n := bytes.Count(data, []byte("\n"))
			_, allowed := longFiles[rel]
			switch {
			case n > maxLines && !allowed:
				t.Errorf("%s is %d lines (over %d): split it by responsibility, or add it to longFiles with why it is one", rel, n, maxLines)
			case n <= maxLines && allowed:
				t.Errorf("%s is %d lines: take it off longFiles", rel, n)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// templates are the engines whose execution looks methods up by name through reflection. A binary that can run one
// keeps every exported method of every reachable type, because the linker can no longer tell which are unused: one
// html/template page once made terma 3 MB (22%) larger. Fill a page with strings.Replacer and html.EscapeString.
var templates = []string{"html/template", "text/template"}

// TestNoTemplateEngine keeps template engines out of the binary: no package of terma's imports one, and the
// terma binary does not link html/template at all, even through a dependency. text/template is linked through
// cobra, which executes it only for a custom template (SetHelpTemplate and the like), so a call to one of those
// would bring the cost back unseen here.
func TestNoTemplateEngine(t *testing.T) {
	t.Parallel()
	binary := false
	for _, p := range listPackages(t) {
		if !within(p.ImportPath, "cmd") && !within(p.ImportPath, "internal") {
			continue
		}
		for _, imp := range p.Imports {
			if slices.Contains(templates, imp) {
				t.Errorf("%s imports %s: it keeps every method in the binary; fill the page with strings.Replacer", p.ImportPath, imp)
			}
		}
		if p.ImportPath == module+"/cmd/terma" {
			binary = true
			if slices.Contains(p.Deps, "html/template") {
				t.Errorf("terma links html/template: it keeps every method in the binary; find what brings it with go mod why html/template")
			}
		}
	}
	if !binary {
		t.Fatal("cmd/terma was not listed: point this test at the binary's package")
	}
}
