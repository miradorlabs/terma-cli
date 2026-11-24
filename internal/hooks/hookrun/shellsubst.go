package hookrun

import (
	"cmp"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type operands struct {
	folders []string
	named   bool
}

// outputs maps known command-substitution prefixes to static path evaluators.
var outputs = map[string]func(sc *scan, args []*syntax.Word) (string, bool){
	"pwd": where, "pwd -L": where, "pwd -P": whereP, "git rev-parse --show-toplevel": toplevel,
	"go env GOPATH": gopath, "go env GOMODCACHE": modcache, "mktemp": mktemp,
	"dirname": ofOperand(dirname), "basename": ofOperand(basename),
	"realpath": ofOperand((*scan).real), "readlink -f": ofOperand((*scan).real),
}

func (sc *scan) substitute(w io.Writer, cs *syntax.CmdSubst) error {
	if len(cs.Stmts) != 1 || len(cs.Stmts[0].Redirs) != 0 {
		return errOpaque
	} else if ce, ok := cs.Stmts[0].Cmd.(*syntax.CallExpr); !ok || len(ce.Assigns) != 0 {
		return errOpaque
	} else {
		words := make([]string, min(len(ce.Args), 4))
		for i := range words {
			words[i] = ce.Args[i].Lit()
		}
		for n := len(words); n > 0; n-- {
			if known, ok := outputs[strings.Join(words[:n], " ")]; ok {
				outer := sc.cfg
				sc.cfg = sc.config() // expand is not reentrant: an operand expands with its own
				out, ok := known(sc, ce.Args[n:])
				sc.cfg = outer
				if !ok || !sc.charge(len(out)) || !sc.chargeFields(out) {
					return errOpaque
				}
				_, err := io.WriteString(w, out)
				return err
			}
		}
	}
	return errOpaque
}
func where(s *scan, a []*syntax.Word) (string, bool) { return s.dir, s.dir != "" && len(a) == 0 }
func whereP(sc *scan, args []*syntax.Word) (string, bool) {
	if sc.dir != "" && len(args) == 0 {
		r, ok := sc.real(sc.dir)
		return r, ok && !sc.unknown
	}
	return "", false
}
func toplevel(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) == 0 {
		return sc.safeLocate(sc.dir)
	}
	return "", false
}
func (sc *scan) hasGoEnv(key string) bool {
	p, _ := sc.lookup("GOENV")
	switch {
	case sc.isUnresolvedVar("GOENV") || p != "" && p != "off" && !filepath.IsAbs(p):
		return true
	case p == "off":
		return false
	case p == "":
		dir := ""
		if runtime.GOOS == "windows" {
			dir, _ = sc.lookup("AppData")
		} else if xdg, _ := sc.lookup("XDG_CONFIG_HOME"); runtime.GOOS != "darwin" && xdg != "" {
			dir = xdg
		} else if home, ok := sc.lookup("HOME"); ok {
			if dir = filepath.Join(home, ".config"); runtime.GOOS == "darwin" {
				dir = filepath.Join(home, "Library", "Application Support")
			}
		}
		if dir == "" {
			return false
		}
		p = filepath.Join(dir, "go", "env")
	}
	return sc.fileHasLine(p, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), key+"=") })
}
func gopath(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOPATH") {
		return "", false
	} else if v, ok := sc.lookup("GOPATH"); ok && v != "" {
		return v, !slices.ContainsFunc(filepath.SplitList(v), func(p string) bool { return !filepath.IsAbs(p) })
	}
	key := "HOME"
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	}
	home, ok := sc.lookup(key)
	return filepath.Join(home, "go"), !sc.isUnresolvedVar(key) && !sc.hasGoEnv("GOPATH") && ok && home != ""
}
func modcache(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOMODCACHE") {
		return "", false
	} else if v, ok := sc.lookup("GOMODCACHE"); ok && v != "" {
		return v, filepath.IsAbs(v)
	} else if gp, ok := gopath(sc, nil); !sc.hasGoEnv("GOMODCACHE") && ok {
		if first, _, _ := strings.Cut(gp, string(os.PathListSeparator)); first != "" {
			return filepath.Join(first, "pkg", "mod"), true
		}
	}
	return "", false
}
func mktemp(sc *scan, args []*syntax.Word) (string, bool) {
	name, tmp := "", false
	for _, a := range args {
		l := a.Lit()
		short := strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--")
		switch {
		case l == "" || strings.HasPrefix(l, "--tmpdir=") || short && strings.ContainsRune(l[1:], 'p') || !strings.HasPrefix(l, "-") && (name != "" || !strings.Contains(l, "XXX") || pathLike(l)):
			return "", false
		case l == "--tmpdir" || short && strings.ContainsRune(l[1:], 't'):
			tmp = true
		case !strings.HasPrefix(l, "-"):
			name = l
		}
	}
	dir := sc.dir
	if name == "" || tmp {
		v, _ := sc.lookup("TMPDIR")
		if dir = cmp.Or(v, os.TempDir()); sc.isUnresolvedVar("TMPDIR") {
			return "", false
		}
	}
	return filepath.Join(dir, cmp.Or(name, "tmp.XXXXXXXXXX")), dir != ""
}
func ofOperand(f func(*scan, string) (string, bool)) func(*scan, []*syntax.Word) (string, bool) {
	return func(sc *scan, args []*syntax.Word) (string, bool) {
		if len(args) > 0 && args[0].Lit() == "--" {
			args = args[1:]
		}
		if len(args) == 1 {
			if fields, err := sc.fields(args[0]); err == nil && len(fields) == 1 {
				return f(sc, fields[0])
			}
		}
		return "", false
	}
}
func basename(_ *scan, p string) (string, bool) {
	if p != "" {
		return filepath.Base(p), true
	}
	return "", true
}
func dirname(_ *scan, p string) (string, bool) {
	vol, sep := filepath.VolumeName(p), "/"
	if filepath.Separator == '\\' {
		sep = `/\`
	}
	rest := strings.TrimRight(p[len(vol):], sep)
	switch i := strings.LastIndexAny(rest, sep); {
	case rest == "" && len(p) > len(vol):
		return vol + string(filepath.Separator), true
	case i < 0:
		return vol + ".", true
	default:
		return vol + filepath.FromSlash(cmp.Or(strings.TrimRight(rest[:i], sep), string(filepath.Separator))), true
	}
}
func (sc *scan) real(p string) (string, bool) {
	if q, ok := sc.path(p, sc.dir); p == "" || !ok || sc.hasRemoteLink(q) {
		return "", false
	} else if r, err := filepath.EvalSymlinks(q); err == nil {
		return r, r != "" && !remote(r, sc.cwd)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false
	} else if _, err := os.Lstat(q); errors.Is(err, fs.ErrNotExist) && filepath.Dir(q) != q && !sc.hasRemoteLink(filepath.Dir(q)) {
		if pr, err := filepath.EvalSymlinks(filepath.Dir(q)); err == nil && pr != "" && !remote(pr, sc.cwd) {
			info, err := os.Stat(pr)
			return filepath.Join(pr, filepath.Base(q)), err == nil && info.IsDir()
		}
	}
	return "", false
}

// safeLocate finds the git worktree root for dir without statting remote targets or crossing mounts.
func (sc *scan) safeLocate(dir string) (string, bool) {
	if dir == "" || sc.hasRemoteLink(dir) ||
		slices.ContainsFunc([]string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM"}, sc.isUnresolvedVar) ||
		slices.ContainsFunc([]string{"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM"}, func(k string) bool { v, _ := sc.lookup(k); return v != "" }) {
		return "", false
	}
	gd, _ := sc.lookup("GIT_DIR")
	wt, _ := sc.lookup("GIT_WORK_TREE")
	if gd != "" || wt != "" {
		base := cmp.Or(sc.dir, sc.cwd)
		absGD, ok1 := sc.resolveTarget(base, gd, 1)
		absWT, ok2 := sc.resolveTarget(base, wt, 1)
		if gd != "" && (!ok1 || sc.hasRemoteLink(absGD)) || wt != "" && (!ok2 || sc.hasRemoteLink(absWT)) || gd == "" || wt == "" {
			return "", false
		}
		info, err := os.Stat(absGD)
		return absWT, err == nil && info.IsDir()
	}
	if start, ok := sc.real(dir); !ok {
		return "", false
	} else if startInfo, err := os.Stat(start); err == nil && startInfo.IsDir() {
		for cur := start; sc.spendStat(1); cur = filepath.Dir(cur) {
			if ci, err := os.Stat(cur); cur != start && (err != nil || !sameDevice(startInfo, ci)) {
				break
			}
			dotGit := filepath.Join(cur, ".git")
			if info, err := os.Lstat(dotGit); err == nil {
				if info.IsDir() {
					return cur, !sc.hasWorktreeConfig(dotGit)
				} else if info.Mode().IsRegular() {
					return sc.locateGitdirFile(dotGit, cur)
				}
				return "", false
			} else if cur == filepath.Dir(cur) {
				break
			}
		}
	}
	return "", false
}
func (sc *scan) hasWorktreeConfig(gitDir string) bool {
	match := func(l string) bool {
		k, _, eq := strings.Cut(l, "=")
		return eq && strings.EqualFold(strings.TrimSpace(k), "worktree")
	}
	return sc.fileHasLine(filepath.Join(gitDir, "config"), match) || sc.fileHasLine(filepath.Join(gitDir, "config.worktree"), match)
}
func (sc *scan) fileHasLine(path string, match func(string) bool) bool {
	p, ok := sc.resolveTarget(sc.dir, path, 0)
	if !ok || sc.hasRemoteLink(p) {
		return true
	}
	data, err := readSmallFile(p)
	return err != nil && !errors.Is(err, fs.ErrNotExist) || err == nil && slices.ContainsFunc(strings.Split(data, "\n"), match)
}
func (sc *scan) isUnresolvedVar(name string) bool { v, set := sc.vars[name]; return set && v == nil }
func readSmallFile(path string) (string, error) {
	f, err := openFileNoBlock(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf [maxPathBytes]byte
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", errOpaque
	} else if n, err := io.ReadFull(f, buf[:]); n == len(buf) || err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", errOpaque
	} else {
		return string(buf[:n]), nil
	}
}
func (sc *scan) locateGitdirFile(path, cur string) (string, bool) {
	if data, err := readSmallFile(path); err != nil {
		sc.unknown = sc.unknown || errors.Is(err, errOpaque)
	} else if line, _, _ := strings.Cut(data, "\n"); strings.HasPrefix(strings.TrimSpace(line), "gitdir:") {
		if target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "gitdir:")); target != "" {
			if resolved, ok := sc.resolveTarget(cur, target, 1); ok && !sc.hasRemoteLink(resolved) {
				info, err := os.Stat(resolved)
				return cur, err == nil && info.IsDir() && !sc.hasWorktreeConfig(resolved)
			}
		}
	}
	return "", false
}
func (sc *scan) below(dir string, levels int) {
	c := candidate{dir, strconv.Itoa(levels), walked}
	if _, seen := sc.examined[c]; seen || !sc.spend(1) {
		return
	}
	if sc.examined[c] = (cached{}); sc.holdsNone(dir) {
		return
	} else if root, ok := sc.safeLocate(dir); ok && !sc.enclosingCheckout(root) || sc.unknown {
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
					} else if info, err := os.Stat(sub); err != nil || !info.IsDir() {
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
		if r, err := filepath.EvalSymlinks(home); err == nil && samePath(root, r) || err != nil && samePath(root, home) {
			return true
		}
	}
	if !sc.cwdRootDone {
		sc.cwdRootDone = true
		sc.cwdRoot, _ = sc.safeLocate(sc.cwd)
	}
	rel, err := filepath.Rel(root, sc.cwdRoot)
	return sc.cwdRoot != "" && !samePath(root, sc.cwdRoot) && err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	} else if f, err := openDirNoBlock(dir); err != nil {
		sc.unknown = sc.unknown || !isBenignDirErr(err)
		return nil
	} else {
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
}
