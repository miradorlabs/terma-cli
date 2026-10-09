package codex

import (
	"io/fs"
	"os"
	"path/filepath"
)

// directoryMovePaths expands a Git directory move while its source is still visible.
// After the source disappears, an existing directory destination cannot tell us which
// files were moved into it, so do not invent a file at either directory's bare path.
func directoryMovePaths(words []word, cwd string, made map[string]bool) ([]string, bool) {
	if len(words) == 0 || filepath.Base(words[0].text) != "mv" {
		return nil, false
	}
	args := make([]string, len(words))
	for i, w := range words {
		args[i] = w.text
	}
	ops := operands(args)
	if len(ops) < 2 {
		return nil, false
	}
	resolve := func(w word) string {
		if !w.literal || w.text == "" || (cwd == "" && !filepath.IsAbs(w.text)) {
			return ""
		}
		if filepath.IsAbs(w.text) {
			return filepath.Clean(w.text)
		}
		return filepath.Join(cwd, w.text)
	}
	dest := resolve(words[ops[len(ops)-1]])
	if dest == "" {
		return nil, false
	}
	destInfo, _ := os.Stat(dest)
	destIsDir := made[dest] || (destInfo != nil && destInfo.IsDir())
	var sources []string
	directory, missing := false, false
	for _, i := range ops[:len(ops)-1] {
		source := resolve(words[i])
		if source == "" {
			return nil, false
		}
		info, err := os.Lstat(source)
		missing = missing || err != nil
		directory = directory || (info != nil && info.IsDir())
		sources = append(sources, source)
	}
	if !directory {
		return nil, missing && destInfo != nil && destInfo.IsDir()
	}
	var out []string
	for _, source := range sources {
		target := dest
		if destIsDir || len(sources) > 1 {
			target = filepath.Join(dest, filepath.Base(source))
		}
		info, err := os.Lstat(source)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			out = append(out, source, target)
			continue
		}
		_ = filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return nil
			}
			relative, err := filepath.Rel(source, path)
			if err == nil {
				out = append(out, path, filepath.Join(target, relative))
			}
			return nil
		})
	}
	return out, true
}
