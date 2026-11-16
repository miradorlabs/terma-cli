package hookrun

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/pattern"

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

// operand notes p, which a word in role r names and info describes, among what the program
// whose words the scan reads names (operands), when r is an operand's: an argument's, a
// folder option's or a fragment of one.
func (sc *scan) operand(p string, info os.FileInfo, r role) {
	if sc.reads == nil || info == nil || r != plain && r != folder && r != part {
		return
	}
	sc.reads.named = true
	if info.IsDir() {
		sc.reads.folders = append(sc.reads.folders, p)
	}
}

// depthOption reports whether opt is name's option bounding how deep it reads (depthOptions).
func depthOption(name, opt string) bool {
	d, ok := depthOptions[name]
	return opt == "-maxdepth" || opt == "--max-depth" || ok && d == opt
}

// maxBelowDepth bounds how far below a folder outside any checkout ShellPaths looks for the
// checkouts a program or a search reading below it reads.
const maxBelowDepth = 4

// below reaches the checkouts below dir, a folder a program or a search reads below, to the
// depth levels whose entries it reads (find -maxdepth; past maxBelowDepth for no bound of
// its own): none when dir is in a checkout, which is its place (one nested in it is not
// looked for), or in a tree known to hold none (holdsNone); else each checkout a walk down at
// most maxBelowDepth levels finds, each folder it reads and each entry a lookup, the trees
// known to hold none skipped. A walk that runs out of either, or past the scan's deadline,
// reaches somewhere not known.
func (sc *scan) below(dir string, levels int) {
	c := candidate{dir, strconv.Itoa(levels), walked}
	if _, seen := sc.examined[c]; seen || !sc.spend(1) {
		return
	}
	sc.examined[c] = ""
	if _, _, ok := gitx.LocateFS(dir); ok || sc.holdsNone(dir) {
		return
	}
	level := []string{dir}
	for depth := 0; len(level) > 0 && !sc.unknown && depth+1 < levels; depth++ {
		var next []string
		for _, d := range level {
			for _, e := range sc.readDir(d) {
				sub := filepath.Join(d, e.Name())
				if e.Type()&fs.ModeSymlink != 0 {
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

// globstar reaches what a glob holding a ** segment (zsh's, or bash's globstar), a word in
// role r, matches from each of dirs, segs its segments: the folder its literal prefix names,
// an operand, and every folder below it, which ** reads (below). False when that folder is
// not there.
func (sc *scan) globstar(dirs, segs []string, r role) bool {
	found := false
	for _, dir := range dirs {
		p := filepath.Join(dir, unescape(literalPrefix(strings.Join(segs, "/"))))
		if !sc.spend(1) {
			break
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			found = true
			sc.place(p)
			sc.operand(p, info, r)
			sc.below(p, maxBelowDepth+2)
		}
	}
	return found || sc.unknown
}

// literalPrefix is the folder a glob's segments name before its first wildcard, its last
// segment never one (src/*.go and src/a.go are src; *.go is none).
func literalPrefix(glob string) string {
	segs := strings.Split(glob, "/")
	n := slices.IndexFunc(segs, func(s string) bool { return pattern.HasMeta(s, 0) })
	if n < 0 || n == len(segs) {
		n = len(segs) - 1
	}
	if n == 1 && segs[0] == "" {
		return "/"
	}
	return strings.Join(segs[:n], "/")
}

// readChunk is how many of a folder's entries readDir reads at a time.
const readChunk = 256

// readDir is what the folder dir holds, by name, each entry read a lookup: none past
// maxShellLookups, which it reads no further than, or its deadline, checked after each
// readChunk entries.
func (sc *scan) readDir(dir string) []fs.DirEntry {
	if !sc.spend(1) {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	var entries []fs.DirEntry
	for room := maxShellLookups - sc.lookups + 1; len(entries) < room; {
		chunk, err := f.ReadDir(min(readChunk, room-len(entries)))
		if entries = append(entries, chunk...); err != nil || len(chunk) == 0 || !sc.spend(0) {
			break
		}
	}
	if !sc.spend(len(entries)) {
		return nil
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries
}
