package hookrun

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// How a scan reads below a folder, a program's or a search's: what a program's words name,
// how deep it reads, the trees known to hold no checkout, and a folder's entries.

// operands are what a program's words name that is there: the folders, which it reads below
// unless it is known not to (run), and whether any names anything.
type operands struct {
	folders []string
	named   bool
}

// maxBelowDepth bounds how far below a folder outside any checkout ShellPaths looks for the
// checkouts a program or a search reading below it reads.
const maxBelowDepth = 4

// below reaches the checkouts below dir, a folder a program or a search reads below, to the
// depth levels whose entries it reads (find -maxdepth; past maxBelowDepth for no bound of
// its own): none when dir is in a checkout (unless that checkout is $HOME or strictly
// encloses the command's own checkout, such as a ~/.git dotfiles repository), or in a tree
// known to hold none (holdsNone); else each checkout a walk down at most maxBelowDepth
// levels finds, each folder it reads and each entry a lookup, the trees known to hold none
// skipped. A walk that runs out of either, or past the scan's deadline, reaches somewhere
// not known.
func (sc *scan) below(dir string, levels int) {
	c := candidate{dir, strconv.Itoa(levels), walked}
	if _, seen := sc.examined[c]; seen || !sc.spend(1) {
		return
	}
	sc.examined[c] = cached{}
	if root, _, ok := gitx.LocateFS(dir); ok && !sc.enclosingCheckout(root) || sc.holdsNone(dir) {
		return
	}
	level := []string{dir}
	for depth := 0; len(level) > 0 && !sc.unknown && depth+1 < levels; depth++ {
		var next []string
		for _, d := range level {
			for _, e := range sc.readDir(d) {
				sub := filepath.Join(d, e.Name())
				if e.Type()&fs.ModeSymlink != 0 {
					if !sc.spend(1) || sc.linkTargetRemote(sub) {
						if sc.unknown {
							return
						}
						continue
					}
					if info, err := os.Stat(sub); err != nil || !info.IsDir() {
						continue
					}
				} else if !e.IsDir() {
					continue
				}
				if depth == maxBelowDepth || !sc.spend(1) {
					sc.unknown = true
					return
				}
				if _, err := os.Lstat(filepath.Join(sub, ".git")); err == nil {
					sc.place(sub)
					continue
				}
				if !sc.holdsNone(sub) {
					next = append(next, sub)
				}
			}
		}
		level = next
	}
}

// enclosingCheckout reports whether root is a checkout that encloses the command's working
// directory without being its own checkout (for example a ~/.git dotfiles repository or a
// parent repository above a nested checkout), so below does not stop at it and miss sibling
// checkouts beneath it.
func (sc *scan) enclosingCheckout(root string) bool {
	if root == "" {
		return false
	}
	if home, ok := sc.lookup("HOME"); ok && home != "" {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		if samePath(root, home) {
			return true
		}
	}
	if !sc.cwdRootDone {
		sc.cwdRootDone = true
		if r, _, ok := gitx.LocateFS(sc.cwd); ok {
			sc.cwdRoot = r
		}
	}
	return sc.cwdRoot != "" && !samePath(root, sc.cwdRoot) && isStrictlyBelow(sc.cwdRoot, root)
}

// isStrictlyBelow reports whether child sits strictly beneath parent.
func isStrictlyBelow(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// holdsNone reports whether p is in a tree known to hold no checkout: a node_modules folder,
// the Go module cache, ~/Library or /System.
func (sc *scan) holdsNone(p string) bool {
	sep := string(filepath.Separator)
	if filepath.Base(p) == "node_modules" || strings.Contains(p, sep+"node_modules"+sep) {
		return true
	}
	if sc.none == nil {
		sc.none = []string{sep + "System"}
		if cache, ok := modcache(sc, nil); ok {
			sc.none = append(sc.none, cache)
		}
		if home, ok := sc.lookup("HOME"); ok && home != "" {
			sc.none = append(sc.none, filepath.Join(home, "Library"))
		}
	}
	return slices.ContainsFunc(sc.none, func(root string) bool {
		return p == root || strings.HasPrefix(p, root+sep)
	})
}

// readChunk is how many of a folder's entries readDir reads at a time.
const readChunk = 256

// readDir is what the folder dir holds, by name, each entry read a lookup: none past
// maxShellLookups, which it reads no further than, or its deadline, checked after each
// readChunk entries. It opens dir with O_DIRECTORY|O_CLOEXEC on Unix so a FIFO or a
// symlink to one never blocks the scan; unexpected open or read errors fail closed (sc.unknown).
func (sc *scan) readDir(dir string) []fs.DirEntry {
	if !sc.spend(1) {
		return nil
	}
	f, err := openDirNoBlock(dir)
	if err != nil {
		if !isBenignDirErr(err) {
			sc.unknown = true
		}
		return nil
	}
	defer f.Close()
	var entries []fs.DirEntry
	for room := maxShellLookups - sc.lookups + 1; len(entries) < room; {
		chunk, err := f.ReadDir(min(readChunk, room-len(entries)))
		entries = append(entries, chunk...)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				sc.unknown = true
				return nil
			}
			break
		}
		if len(chunk) == 0 || !sc.spend(0) {
			break
		}
	}
	if !sc.spend(len(entries)) {
		return nil
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries
}
