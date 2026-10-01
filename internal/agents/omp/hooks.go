package omp

import (
	_ "embed"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// hooksPath is terma's file in .omp/hooks/pre, where omp loads repository hooks.
const hooksPath = ".omp/hooks/pre/terma.ts"

//go:embed hooks.ts
var ompHooksSource string

func planHooks(root string, install bool) (hookmgr.Plan, error) {
	return mergeOmpHooks(root, install)
}

// mergeOmpHooks plans terma's hook file, written whole, in or out of a repository;
// uninstall removes it only while it matches terma's render, since an edited file is theirs.
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
		p.Notes = append(p.Notes, hooksPath+" has local edits — left in place")
	}
	return p, nil
}

func sameFile(a, b []byte) bool {
	return string(a) == string(b)
}

// hasConfig reports whether the repository has a .omp directory, so wiring its hooks by
// default adds no stray directory.
func hasConfig(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".omp"))
	return err == nil && info.IsDir()
}
