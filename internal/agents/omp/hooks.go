package omp

import (
	_ "embed"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// hooksPath is where omp loads repository hooks: a .omp/hooks/pre directory, one
// TypeScript file per concern. terma's file installs the attribution wiring — session
// lifecycle and file edits handed to `terma hook omp-*`.
const hooksPath = ".omp/hooks/pre/terma.ts"

//go:embed hooks.ts
var ompHooksSource string

// planHooks plans terma's hook file in or out of the repository. The file is
// generated whole, so install rewrites it and uninstall removes it only when it still
// matches a terma render — a file somebody edited is theirs to keep.
func planHooks(root string, install bool) (hookmgr.Plan, error) {
	return mergeOmpHooks(root, install)
}

// mergeOmpHooks plans terma's hook file in or out of a repository. Unlike the JSON
// agents, omp's hook file is a TypeScript source: install writes the embedded file
// with the event list spliced in, and uninstall removes the file only when it still
// matches a terma render — a file somebody edited is theirs to keep.
func mergeOmpHooks(root string, install bool) (hookmgr.Plan, error) {
	p := hookmgr.Plan{}
	before, err := hookmgr.ReadFile(filepath.Join(root, filepath.FromSlash(hooksPath)))
	if err != nil {
		return p, err
	}
	after := []byte(ompHooksSource)

	switch {
	case install && before == nil:
		p.Changes = append(p.Changes, hookmgr.Change{Path: hooksPath, After: after})
	case install && !sameFile(before, after):
		p.Changes = append(p.Changes, hookmgr.Change{Path: hooksPath, Before: before, After: after})
	case !install && before != nil && sameFile(before, after):
		p.Changes = append(p.Changes, hookmgr.Change{Path: hooksPath, Before: before})
	case !install && before != nil:
		// A file that no longer matches the render has been edited since install.
		// It is not terma's to delete; say so rather than leave the impression it was.
		p.Notes = append(p.Notes, hooksPath+" has local edits — left in place")
	}
	return p, nil
}

// sameFile reports whether two byte slices name the same content. Kept separate from
// bytes.Equal so the comparison can grow normalization (trailing newline, BOM) without
// touching every caller.
func sameFile(a, b []byte) bool {
	return string(a) == string(b)
}

// hasConfig reports whether the repository already carries omp configuration — a .omp
// directory — which is when wiring its hooks by default is a help rather than a stray
// directory in a repository nobody opens in omp.
func hasConfig(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".omp"))
	return err == nil && info.IsDir()
}
