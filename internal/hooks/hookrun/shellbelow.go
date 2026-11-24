package hookrun

import (
	"cmp"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type operands struct {
	folders []string
	named   bool
}

// safeLocate finds the git worktree root for dir without statting remote GIT_DIR or .git gitdir targets.
func (sc *scan) safeLocate(dir string) (string, bool) {
	if dir == "" || sc.hasRemoteLink(dir) || sc.isUnresolvedVar("GIT_DIR") || sc.isUnresolvedVar("GIT_WORK_TREE") {
		return "", false
	}
	if gd, ok := sc.lookup("GIT_DIR"); ok && gd != "" {
		wt, _ := sc.lookup("GIT_WORK_TREE")
		base := cmp.Or(sc.dir, sc.cwd)
		absGD, ok1 := sc.resolveTarget(base, gd, 1)
		absWT, ok2 := sc.resolveTarget(base, cmp.Or(wt, dir), 1)
		if !ok1 || !ok2 || sc.hasRemoteLink(absGD) || sc.hasRemoteLink(absWT) {
			return "", false
		}
		info, err := os.Stat(absGD)
		return absWT, err == nil && info.IsDir()
	}
	if wt, ok := sc.lookup("GIT_WORK_TREE"); ok && wt != "" {
		return "", false
	}
	for cur, err := filepath.Abs(dir); err == nil && sc.spendStat(1); cur = filepath.Dir(cur) {
		dotGit := filepath.Join(cur, ".git")
		if info, err := os.Lstat(dotGit); err == nil {
			if info.IsDir() {
				return cur, true
			} else if info.Mode().IsRegular() {
				return sc.locateGitdirFile(dotGit, cur)
			}
			return "", false
		}
		if cur == filepath.Dir(cur) {
			break
		}
	}
	return "", false
}
func (sc *scan) isUnresolvedVar(name string) bool { v, set := sc.vars[name]; return set && v == nil }
func readSmallFile(path string) (string, error) {
	f, err := openFileNoBlock(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", errOpaque
	}
	var buf [maxPathBytes]byte
	if n, err := io.ReadFull(f, buf[:]); n == len(buf) || err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", errOpaque
	} else {
		return string(buf[:n]), nil
	}
}
func (sc *scan) locateGitdirFile(path, cur string) (string, bool) {
	data, err := readSmallFile(path)
	if err != nil {
		sc.unknown = sc.unknown || errors.Is(err, errOpaque)
		return "", false
	}
	line, _, _ := strings.Cut(data, "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if target = strings.TrimSpace(target); ok && target != "" {
		if resolved, ok := sc.resolveTarget(cur, target, 1); ok && !sc.hasRemoteLink(resolved) {
			info, err := os.Stat(resolved)
			return cur, err == nil && info.IsDir()
		}
	}
	return "", false
}
func (sc *scan) below(dir string, levels int) {
	c := candidate{dir, strconv.Itoa(levels), walked}
	if _, seen := sc.examined[c]; seen || !sc.spend(1) {
		return
	}
	sc.examined[c] = cached{}
	if sc.holdsNone(dir) {
		return
	}
	if root, ok := sc.safeLocate(dir); ok && !sc.enclosingCheckout(root) || sc.unknown {
		return
	}
	for depth, level := 0, []string{dir}; len(level) > 0 && !sc.unknown && depth+1 < levels; depth++ {
		var next []string
		for _, d := range level {
			for _, e := range sc.readDir(d) {
				sub := filepath.Join(d, e.Name())
				if e.Type()&fs.ModeSymlink != 0 {
					if _, bad := sc.resolveLink(sub, 0); bad {
						return
					}
					if info, err := os.Stat(sub); err != nil || !info.IsDir() {
						continue
					}
				} else if !e.IsDir() {
					continue
				}
				if depth == maxBelowDepth && sc.fail() || !sc.spend(1) {
					return
				}
				if _, err := os.Lstat(filepath.Join(sub, ".git")); err == nil {
					sc.place(sub)
				} else if !sc.holdsNone(sub) {
					next = append(next, sub)
				}
			}
		}
		level = next
	}
}
func (sc *scan) enclosingCheckout(root string) bool {
	if home, ok := sc.lookup("HOME"); ok && home != "" && !sc.hasRemoteLink(home) {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		if samePath(root, home) {
			return true
		}
	}
	if !sc.cwdRootDone {
		sc.cwdRootDone = true
		sc.cwdRoot, _ = sc.safeLocate(sc.cwd)
	}
	if sc.cwdRoot == "" || samePath(root, sc.cwdRoot) {
		return false
	}
	rel, err := filepath.Rel(root, sc.cwdRoot)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
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
	return slices.ContainsFunc(sc.none, func(root string) bool { return p == root || strings.HasPrefix(p, root+sep) })
}

// readDir lists dir via O_DIRECTORY|O_CLOEXEC (FIFO-safe), failing closed on unexpected errors.
func (sc *scan) readDir(dir string) []fs.DirEntry {
	if !sc.spend(1) {
		return nil
	}
	f, err := openDirNoBlock(dir)
	if err != nil {
		sc.unknown = sc.unknown || !isBenignDirErr(err)
		return nil
	}
	defer f.Close()
	var entries []fs.DirEntry
	for room := maxShellLookups - sc.lookups + 1; len(entries) < room; {
		chunk, err := f.ReadDir(min(256, room-len(entries)))
		if entries = append(entries, chunk...); errors.Is(err, io.EOF) || err == nil && (len(chunk) == 0 || !sc.spend(0)) {
			break
		} else if err != nil && sc.fail() {
			return nil
		}
	}
	if !sc.spend(len(entries)) {
		return nil
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries
}
