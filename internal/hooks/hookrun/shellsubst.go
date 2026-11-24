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
	"pwd": where, "pwd -L": where, "pwd -P": func(sc *scan, a []*syntax.Word) (string, bool) { return sc.real(pick(len(a) == 0, sc.dir, "")) },
	"git rev-parse --show-toplevel": func(sc *scan, a []*syntax.Word) (string, bool) { return sc.safeLocate(pick(len(a) == 0, sc.dir, "")) },
	"mktemp":                        mktemp, "go env GOPATH": gopath, "go env GOMODCACHE": modcache, "dirname": ofOperand(dirname),
	"basename": ofOperand(func(_ *scan, p string) (string, bool) { return pick(p != "", filepath.Base(p), ""), true }),
	"realpath": ofOperand((*scan).real), "readlink -f": ofOperand((*scan).real),
}

func (sc *scan) substitute(w io.Writer, cs *syntax.CmdSubst) error {
	if len(cs.Stmts) != 1 || len(cs.Stmts[0].Redirs) != 0 {
		return errOpaque
	}
	if ce, ok := cs.Stmts[0].Cmd.(*syntax.CallExpr); ok && len(ce.Assigns) == 0 {
		words := make([]string, min(len(ce.Args), 4))
		for i := range words {
			words[i] = ce.Args[i].Lit()
		}
		for n := len(words); n > 0; n-- {
			if known, ok := outputs[strings.Join(words[:n], " ")]; ok {
				outer := sc.cfg
				sc.cfg = sc.config() // expand is not reentrant: an operand expands with its own
				out, ok := known(sc, ce.Args[n:])
				if sc.cfg, out = outer, strings.TrimRight(out, "\n"); !ok || !sc.charge(len(out)) || !sc.chargeFields(out) {
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
func (sc *scan) envVal(k string) string              { v, _ := sc.lookup(k); return v }
func (sc *scan) hasGoEnv(key string) bool {
	p := sc.envVal("GOENV")
	if sc.isUnresolvedVar("GOENV") || p != "" && p != "off" && !filepath.IsAbs(p) {
		return true
	} else if p == "off" {
		return false
	} else if p == "" {
		dir, bad := "", false
		if runtime.GOOS == "windows" {
			dir, bad = sc.envVal("AppData"), sc.isUnresolvedVar("AppData")
		} else if xdg := sc.envVal("XDG_CONFIG_HOME"); runtime.GOOS != "darwin" && (xdg != "" || sc.isUnresolvedVar("XDG_CONFIG_HOME")) {
			dir, bad = xdg, sc.isUnresolvedVar("XDG_CONFIG_HOME")
		} else if home, ok := sc.lookup("HOME"); ok || sc.isUnresolvedVar("HOME") {
			dir, bad = filepath.Join(home, pick(runtime.GOOS == "darwin", "Library/Application Support", ".config")), sc.isUnresolvedVar("HOME")
		}
		if p = filepath.Join(dir, "go", "env"); bad || dir == "" {
			return bad
		}
	}
	return sc.fileHasLine(p, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), key+"=") })
}
func gopath(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOPATH") {
		return "", false
	} else if v, ok := sc.lookup("GOPATH"); ok && v != "" {
		return v, !slices.ContainsFunc(filepath.SplitList(v), func(p string) bool { return !filepath.IsAbs(p) })
	}
	key := pick(runtime.GOOS == "windows", "USERPROFILE", "HOME")
	home, ok := sc.lookup(key)
	return filepath.Join(home, "go"), !sc.isUnresolvedVar(key) && !sc.hasGoEnv("GOPATH") && ok && filepath.IsAbs(home)
}
func modcache(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOMODCACHE") {
		return "", false
	} else if v, ok := sc.lookup("GOMODCACHE"); ok && v != "" {
		return v, filepath.IsAbs(v)
	} else if gp, ok := gopath(sc, nil); !sc.hasGoEnv("GOMODCACHE") && ok {
		first, _, _ := strings.Cut(gp, string(os.PathListSeparator))
		return filepath.Join(first, "pkg", "mod"), first != ""
	}
	return "", false
}
func mktemp(sc *scan, args []*syntax.Word) (string, bool) {
	name, tmp, opts := "", false, true
	for _, a := range args {
		if l := a.Lit(); l == "" || opts && l != "--" && strings.HasPrefix(l, "-") && l != "--tmpdir" && l != "--directory" && l != "--quiet" && l != "--dry-run" && (len(l) < 2 || l[1] == '-' || strings.Trim(l[1:], "dqut") != "") {
			return "", false
		} else if opts && l == "--" {
			opts = false
		} else if opts && strings.HasPrefix(l, "-") {
			tmp = tmp || l == "--tmpdir" || l[1] != '-' && strings.ContainsRune(l[1:], 't')
		} else if opts = false; name != "" || !strings.Contains(l, "XXX") || pathLike(l) {
			return "", false
		} else {
			name = l
		}
	}
	dir := pick(name == "" || tmp, cmp.Or(sc.envVal("TMPDIR"), os.TempDir()), sc.dir)
	if (name == "" || tmp) && sc.isUnresolvedVar("TMPDIR") {
		return "", false
	}
	_, _, info, found := sc.walkPath(dir, true)
	return filepath.Join(dir, cmp.Or(name, "tmp.XXXXXXXXXX")), dir != "" && found && info.IsDir()
}
func ofOperand(f func(*scan, string) (string, bool)) func(*scan, []*syntax.Word) (string, bool) {
	return func(sc *scan, args []*syntax.Word) (string, bool) {
		ended := len(args) > 0 && args[0].Lit() == "--"
		if args = args[pick(ended, 1, 0):]; len(args) == 1 {
			if fields, err := sc.fields(args[0]); err == nil && len(fields) == 1 && (ended || !strings.HasPrefix(fields[0], "-")) {
				return f(sc, fields[0])
			}
		}
		return "", false
	}
}
func dirname(_ *scan, p string) (string, bool) {
	vol, sep := filepath.VolumeName(p), pick(filepath.Separator == '\\', `/\`, "/")
	rest := strings.TrimRight(p[len(vol):], sep)
	if i := strings.LastIndexAny(rest, sep); i >= 0 {
		return vol + filepath.FromSlash(cmp.Or(strings.TrimRight(rest[:i], sep), string(filepath.Separator))), true
	} else if rest == "" && len(p) > len(vol) {
		return vol + string(filepath.Separator), true
	}
	return vol + ".", true
}
func (sc *scan) real(p string) (string, bool) {
	if q, ok := sc.path(p, sc.dir); p != "" && ok {
		r, parent, _, found := sc.walkPath(q, true)
		return r, found || !sc.unknown && r != "" && parent != "" && filepath.Dir(r) == parent
	}
	return "", false
}

// safeLocate finds the git worktree root for dir without statting remote targets or crossing mounts.
func (sc *scan) safeLocate(dir string) (string, bool) {
	gitEnv := []string{"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY"}
	if dir == "" || sc.hasRemoteLink(dir) || sc.isUnresolvedVar("GIT_DIR") || sc.isUnresolvedVar("GIT_WORK_TREE") ||
		slices.ContainsFunc(gitEnv, func(k string) bool { return sc.isUnresolvedVar(k) || sc.envVal(k) != "" }) {
		return "", false
	}
	if gd, wt := sc.envVal("GIT_DIR"), sc.envVal("GIT_WORK_TREE"); gd != "" || wt != "" {
		absGD, ok1 := sc.resolveJoined(cmp.Or(sc.dir, sc.cwd), gd, false)
		absWT, ok2 := sc.resolveJoined(cmp.Or(sc.dir, sc.cwd), wt, false)
		return absWT, gd != "" && wt != "" && ok1 && ok2 && !sc.hasRemoteLink(absGD) && !sc.hasRemoteLink(absWT) && sc.validGitDir(absGD)
	}
	if start, ok := sc.real(dir); ok {
		_, _, startInfo, found := sc.walkPath(start, true)
		for cur := start; found && startInfo.IsDir() && sc.spendStat(1); cur = filepath.Dir(cur) {
			if _, _, ci, ok := sc.walkPath(cur, true); cur != start && (!ok || !sameDevice(startInfo, ci)) {
				break
			}
			dotGit := filepath.Join(cur, ".git")
			if _, _, info, ok := sc.walkPath(dotGit, false); ok {
				if info.IsDir() {
					return cur, sc.validGitDir(dotGit) && !sc.hasWorktreeConfig(dotGit)
				} else if info.Mode().IsRegular() {
					if data, err := sc.readSmallFile(dotGit); err != nil {
						sc.unknown = sc.unknown || errors.Is(err, errOpaque)
					} else if target, ok := strings.CutPrefix(strings.TrimSuffix(strings.TrimSuffix(data, "\n"), "\r"), "gitdir: "); ok && target != "" {
						if resolved, ok := sc.resolveJoined(cur, target, false); ok {
							if r, _, gInfo, gFound := sc.walkPath(resolved, true); gFound && gInfo.IsDir() && sc.validGitDir(r) {
								return cur, !sc.hasWorktreeConfig(r)
							}
						}
					}
				}
				return "", false
			} else if cur == filepath.Dir(cur) {
				break
			}
		}
	}
	return "", false
}
func (sc *scan) validGitDir(dir string) bool {
	head, err := sc.readSmallFile(filepath.Join(dir, "HEAD"))
	if h := strings.TrimSpace(head); err != nil || (!strings.HasPrefix(h, "ref:") || strings.TrimSpace(h[4:]) == "") && (len(h) != 40 && len(h) != 64 || strings.Trim(h, "0123456789abcdefABCDEF") != "") {
		return false
	}
	hasDir := func(p string) bool { _, _, info, ok := sc.walkPath(p, true); return ok && info.IsDir() }
	if hasDir(filepath.Join(dir, "objects")) && (hasDir(filepath.Join(dir, "refs")) || hasDir(filepath.Join(dir, "reftable"))) {
		return true
	}
	if common, err := sc.readSmallFile(filepath.Join(dir, "commondir")); err == nil && strings.TrimSpace(common) != "" {
		r, ok := sc.resolveJoined(dir, strings.TrimSpace(common), false)
		return ok && hasDir(filepath.Join(r, "objects"))
	}
	return false
}
func (sc *scan) hasWorktreeConfig(gitDir string) bool {
	if sc.isUnresolvedVar("HOME") || sc.isUnresolvedVar("USERPROFILE") || sc.isUnresolvedVar("XDG_CONFIG_HOME") || sc.isUnresolvedVar("ProgramData") {
		return true
	}
	match := func(l string) bool {
		t := strings.TrimSpace(strings.TrimPrefix(l, "\ufeff"))
		_, after, bracket := strings.Cut(t, "]")
		f := strings.FieldsFunc(pick(strings.HasPrefix(t, "[") && bracket, after, t), func(r rune) bool { return r == '=' || r <= ' ' })
		return strings.HasPrefix(strings.ToLower(t), "[include") || len(f) > 0 && strings.EqualFold(f[0], "worktree")
	}
	paths := []string{filepath.Join(gitDir, "config"), filepath.Join(gitDir, "config.worktree")}
	if filepath.Separator == '/' || sc.envVal("ProgramData") != "" {
		paths = append(paths, pick(filepath.Separator == '/', "/etc/gitconfig", filepath.Join(sc.envVal("ProgramData"), "Git", "config")))
	}
	if xdg := sc.envVal("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		paths = append(paths, filepath.Join(xdg, "git", "config"))
	}
	if home, ok := sc.homeDir(); ok && home != "" {
		paths = append(paths, filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config"))
	}
	return slices.ContainsFunc(paths, func(p string) bool { return sc.fileHasLine(p, match) })
}
func (sc *scan) fileHasLine(path string, match func(string) bool) bool {
	if p, ok := sc.resolveJoined(sc.dir, path, false); ok {
		data, err := sc.readSmallFile(p)
		return err != nil && !errors.Is(err, fs.ErrNotExist) || err == nil && slices.ContainsFunc(strings.Split(data, "\n"), match)
	}
	return true
}
func (sc *scan) isUnresolvedVar(name string) bool { v, set := sc.vars[name]; return set && v == nil }
func (sc *scan) readSmallFile(path string) (string, error) {
	_, _, info, f, ok := sc.walk(path, true, 1)
	if !ok {
		return "", pick(sc.unknown || info != nil, errOpaque, fs.ErrNotExist)
	}
	defer f.Close()
	var buf [maxPathBytes]byte
	n, err := io.ReadFull(f, buf[:])
	if n == len(buf) || err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", errOpaque
	}
	return string(buf[:n]), nil
}
func (sc *scan) below(dir string, levels int) {
	c := candidate{dir, strconv.Itoa(levels), walked}
	if _, seen := sc.examined[c]; seen || !sc.spend(1) {
		return
	} else if sc.examined[c] = (cached{}); sc.holdsNone(dir) {
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
					if _, _, info, found := sc.walkPath(sub, true); sc.unknown {
						return
					} else if !found || !info.IsDir() {
						continue
					}
				} else if !e.IsDir() {
					continue
				}
				if depth == maxBelowDepth && sc.fail() || !sc.spend(1) {
					return
				}
				if _, _, _, ok := sc.walkPath(filepath.Join(sub, ".git"), false); ok {
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
	if home, ok := sc.homeDir(); ok && home != "" {
		if r, _, _, found := sc.walkPath(home, true); found && samePath(root, r) || !found && !sc.unknown && samePath(root, home) {
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
	if sc.none == nil {
		sc.none = pick(runtime.GOOS == "darwin", []string{sep + "System"}, []string{})
		if cache, ok := modcache(sc, nil); ok {
			sc.none = append(sc.none, cache)
		}
		if home, ok := sc.homeDir(); runtime.GOOS == "darwin" && ok && home != "" {
			sc.none = append(sc.none, filepath.Join(home, "Library"))
		}
	}
	return filepath.Base(p) == "node_modules" || strings.Contains(p, sep+"node_modules"+sep) ||
		slices.ContainsFunc(sc.none, func(root string) bool { return p == root || strings.HasPrefix(p, root+sep) })
}

// readDir lists dir via validated dirHandle (FIFO- and TOCTOU-safe), failing closed on unexpected errors.
func (sc *scan) readDir(dir string) []fs.DirEntry {
	if !sc.spend(1) {
		return nil
	} else if _, _, _, f, ok := sc.walk(dir, true, 2); ok {
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
		if sc.spend(len(entries)) {
			slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			return entries
		}
	}
	return nil
}
