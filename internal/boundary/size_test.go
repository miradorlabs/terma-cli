package boundary

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxLines is where a source file has usually grown a second job.
const maxLines = 450

// longFiles hold one job that reads best whole.
var longFiles = map[string]string{
	"internal/session/session.go":   "the store: every writer under one lock, read together",
	"internal/agents/codex/toml.go": "the [otel] splice, verified before it is written, reviewed as one",
}

// No source file outgrows its job: split one past maxLines by responsibility, or say here
// why it is one.
func TestNoSourceFileOutgrowsItsJob(t *testing.T) {
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
