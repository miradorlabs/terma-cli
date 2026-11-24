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

// outputs maps known command-substitution prefixes to static path evaluators.
var outputs = map[string]func(sc *scan, args []*syntax.Word) (string, bool){
	"pwd": where, "pwd -L": where, "pwd -P": func(sc *scan, a []*syntax.Word) (string, bool) { return sc.real(pick(len(a) == 0, sc.dir, "")) },
	"git rev-parse --show-toplevel": func(sc *scan, a []*syntax.Word) (string, bool) { return sc.safeLocate(pick(len(a) == 0, sc.dir, "")) },
	"mktemp":                        mktemp, "go env GOPATH": gopath, "go env GOMODCACHE": modcache, "dirname": ofOperand(dirname), "realpath": ofOperand((*scan).real), "readlink -f": ofOperand((*scan).real), "basename": ofOperand(func(_ *scan, p string) (string, bool) { return pick(p != "", filepath.Base(p), ""), true }),
}

func (sc *scan) substitute(w io.Writer, cs *syntax.CmdSubst) error {
	if len(cs.Stmts) == 1 && len(cs.Stmts[0].Redirs) == 0 {
		if ce, ok := cs.Stmts[0].Cmd.(*syntax.CallExpr); ok && len(ce.Assigns) == 0 {
			words := make([]string, min(len(ce.Args), 4))
			for i := range words {
				words[i] = ce.Args[i].Lit()
			}
			for n := len(words); n > 0; n-- {
				if known, ok := outputs[strings.Join(words[:n], " ")]; ok {
					if _, ok := sc.trustedCmd(words[0]); ok {
						outer := sc.cfg
						sc.cfg = sc.config() // expand is not reentrant
						out, ok := known(sc, ce.Args[n:])
						if sc.cfg, out = outer, strings.TrimRight(out, "\n"); ok && sc.charge(len(out)) && sc.chargeFields(out) {
							_, err := io.WriteString(w, out)
							return err
						}
					}
					return errOpaque
				}
			}
		}
	}
	return errOpaque
}
func (sc *scan) trustedCmd(name string) (string, bool) {
	pathEnv, hasPath := sc.lookup("PATH")
	if bad := slices.ContainsFunc([]string{"BASH_FUNC_" + name + "%%", "BASH_FUNC_" + name + "()"}, func(k string) bool { _, ok := sc.env(k); return ok }) || sc.isUnresolvedVar("BASH_ENV") || sc.envVal("BASH_ENV") != "" || sc.isUnresolvedVar("ENV") || sc.envVal("ENV") != ""; bad || name == "pwd" || !hasPath && !sc.isUnresolvedVar("PATH") {
		return "", !bad
	}
	exts, roots := pick(runtime.GOOS == "windows", []string{"", ".exe", ".cmd", ".bat", ".com"}, []string{""}), pick(runtime.GOOS == "windows", []string{"/program files/", "/program files (x86)/", "/windows/system32/"}, []string{"/bin/", "/usr/bin/", "/usr/lib/", "/usr/local/bin/", "/Library/Developer/", "/Applications/Xcode.app/", "/usr/local/go/bin/"})
	for _, d := range pick(sc.isUnresolvedVar("PATH") || pathEnv == "", []string{""}, filepath.SplitList(pathEnv)) {
		if d == "" || !filepath.IsAbs(d) {
			return "", false
		}
		for _, ext := range exts {
			if r, parent, info, found := sc.walkPath(filepath.Join(d, name+ext), true); sc.unknown {
				return "", false
			} else if found && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				p := filepath.ToSlash(r[len(filepath.VolumeName(r)):])
				p, same := pick(runtime.GOOS == "windows", strings.ToLower(p), p), pick(runtime.GOOS == "windows", strings.EqualFold(filepath.Base(r), name+ext), filepath.Base(r) == name)
				_, _, pi, pOK := sc.walkPath(parent, true)
				return r, same && isSysBin(info) && pOK && isSysBin(pi) && slices.ContainsFunc(roots, func(pre string) bool { return strings.HasPrefix(p, pre) })
			}
		}
	}
	return "", false
}
func where(s *scan, a []*syntax.Word) (string, bool) { return s.dir, s.dir != "" && len(a) == 0 }
func (sc *scan) envVal(k string) string              { v, _ := sc.lookup(k); return v }
func (sc *scan) envSet(k string) bool                { _, ok := sc.lookup(k); return ok }
func (sc *scan) hasGoEnv(key string) bool {
	p, bad := sc.envVal("GOENV"), sc.isUnresolvedVar("GOENV")
	if !bad && p == "" {
		dir, dBad := sc.envVal("AppData"), sc.isUnresolvedVar("AppData")
		if xdg := sc.envVal("XDG_CONFIG_HOME"); runtime.GOOS != "windows" && runtime.GOOS != "darwin" && (xdg != "" || sc.isUnresolvedVar("XDG_CONFIG_HOME")) {
			dir, dBad = xdg, sc.isUnresolvedVar("XDG_CONFIG_HOME")
		} else if home, ok := sc.lookup("HOME"); runtime.GOOS != "windows" {
			dir, dBad = pick(ok, filepath.Join(home, pick(runtime.GOOS == "darwin", "Library/Application Support", ".config")), ""), sc.isUnresolvedVar("HOME")
		}
		p, bad = pick(dir != "", filepath.Join(dir, "go", "env"), ""), dBad
	}
	return bad || p != "" && p != "off" && (!filepath.IsAbs(p) || sc.fileHasLine(p, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), key+"=") }))
}
func gopath(sc *scan, args []*syntax.Word) (string, bool) {
	if v, ok := sc.lookup("GOPATH"); len(args) != 0 || sc.isUnresolvedVar("GOFLAGS") || sc.envVal("GOFLAGS") != "" || sc.isUnresolvedVar("GOPATH") || ok && v != "" {
		ok = len(args) == 0 && !sc.isUnresolvedVar("GOFLAGS") && sc.envVal("GOFLAGS") == "" && !sc.hasGoEnv("GOFLAGS") && !sc.isUnresolvedVar("GOPATH") && ok && v != "" && !slices.ContainsFunc(filepath.SplitList(v), func(p string) bool { return !filepath.IsAbs(p) })
		return pick(ok, v, ""), ok
	}
	key := pick(runtime.GOOS == "windows", "USERPROFILE", "HOME")
	home, ok := sc.lookup(key)
	return filepath.Join(home, "go"), !sc.isUnresolvedVar(key) && !sc.hasGoEnv("GOPATH") && !sc.hasGoEnv("GOFLAGS") && ok && filepath.IsAbs(home)
}
func modcache(sc *scan, args []*syntax.Word) (string, bool) {
	if v, ok := sc.lookup("GOMODCACHE"); len(args) != 0 || sc.isUnresolvedVar("GOFLAGS") || sc.envVal("GOFLAGS") != "" || sc.isUnresolvedVar("GOMODCACHE") || ok && v != "" {
		ok = len(args) == 0 && !sc.isUnresolvedVar("GOFLAGS") && sc.envVal("GOFLAGS") == "" && !sc.hasGoEnv("GOFLAGS") && !sc.isUnresolvedVar("GOMODCACHE") && ok && filepath.IsAbs(v)
		return pick(ok, v, ""), ok
	} else if gp, ok := gopath(sc, nil); !sc.hasGoEnv("GOMODCACHE") && ok {
		first, _, _ := strings.Cut(gp, string(os.PathListSeparator))
		return filepath.Join(first, "pkg", "mod"), first != ""
	}
	return "", false
}
func mktemp(sc *scan, args []*syntax.Word) (string, bool) {
	name, tmp, opts := "", false, true
	for _, a := range args {
		if l := a.Lit(); l == "" || opts && l != "--" && strings.HasPrefix(l, "-") && !slices.Contains([]string{"--tmpdir", "--directory", "--quiet", "--dry-run"}, l) && (len(l) < 2 || l[1] == '-' || strings.Trim(l[1:], "dqut") != "") {
			return "", false
		} else if opts && strings.HasPrefix(l, "-") {
			opts, tmp = l != "--", tmp || l == "--tmpdir" || l[1] != '-' && strings.ContainsRune(l[1:], 't')
		} else if opts, name = false, pick(name == "" && strings.Contains(l, "XXX") && !pathLike(l), l, ""); name == "" {
			return "", false
		}
	}
	if (name == "" || tmp) && (sc.isUnresolvedVar("TMPDIR") || runtime.GOOS == "windows" && (sc.isUnresolvedVar("TMP") || sc.isUnresolvedVar("TEMP") || sc.isUnresolvedVar("USERPROFILE"))) {
		return "", false
	}
	dir, base := pick(name == "" || tmp, pick(runtime.GOOS == "windows", cmp.Or(sc.envVal("TMP"), sc.envVal("TEMP"), sc.envVal("USERPROFILE")), cmp.Or(sc.envVal("TMPDIR"), "/tmp")), sc.dir), cmp.Or(name, "tmp.XXXXXXXXXX")
	if rDir, _, info, found := sc.walkPath(dir, true); dir != "" && found && info.IsDir() {
		sc.created[pick(runtime.GOOS == "windows", strings.ToLower(filepath.Join(rDir, base)), filepath.Join(rDir, base))] = true
		return filepath.Join(dir, base), true
	}
	return "", false
}
func ofOperand(f func(*scan, string) (string, bool)) func(*scan, []*syntax.Word) (string, bool) {
	return func(sc *scan, args []*syntax.Word) (string, bool) {
		if ended := len(args) > 0 && args[0].Lit() == "--"; len(args) == pick(ended, 2, 1) {
			if fields, err := sc.fields(args[pick(ended, 1, 0)]); err == nil && len(fields) == 1 && (ended || !strings.HasPrefix(fields[0], "-")) {
				return f(sc, fields[0])
			}
		}
		return "", false
	}
}
func dirname(_ *scan, p string) (string, bool) {
	vol, sep := filepath.VolumeName(p), pick(filepath.Separator == '\\', `/\`, "/")
	rest := strings.TrimRight(p[len(vol):], sep)
	if j := strings.LastIndexAny(rest, sep); j >= 0 {
		return vol + filepath.FromSlash(cmp.Or(strings.TrimRight(rest[:j], sep), string(filepath.Separator))), true
	}
	return vol + pick(rest == "" && len(p) > len(vol), string(filepath.Separator), "."), true
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
	if dir == "" || sc.hasRemoteLink(dir) || sc.isUnresolvedVar("GIT_DIR") || sc.isUnresolvedVar("GIT_WORK_TREE") || sc.envSet("GIT_COMMON_DIR") || sc.envSet("GIT_OBJECT_DIRECTORY") || slices.ContainsFunc([]string{"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "SUDO_UID"}, func(k string) bool { return sc.isUnresolvedVar(k) || sc.envVal(k) != "" }) {
		return "", false
	}
	if gd, wt := sc.envVal("GIT_DIR"), sc.envVal("GIT_WORK_TREE"); sc.envSet("GIT_DIR") || sc.envSet("GIT_WORK_TREE") {
		absGD, ok1 := sc.resolveJoined(cmp.Or(sc.dir, sc.cwd), gd, false)
		absWT, ok2 := sc.resolveJoined(cmp.Or(sc.dir, sc.cwd), wt, false)
		if gd != "" && wt != "" && ok1 && ok2 && !sc.hasRemoteLink(absGD) && !sc.hasRemoteLink(absWT) {
			rw, _, wi, okW := sc.walkPath(absWT, true)
			ok := okW && sameOwner(rw, wi) && sc.validGitDir(absGD, true) && !sc.hasWorktreeConfig(absGD)
			return pick(ok, absWT, ""), ok
		}
		return "", false
	}
	if start, ok := sc.real(dir); ok {
		_, _, startInfo, found := sc.walkPath(start, true)
		for cur := start; found && startInfo.IsDir() && sc.spendStat(1); cur = filepath.Dir(cur) {
			rc, _, ci, ok := sc.walkPath(cur, true)
			if !ok || cur != start && !sameDevice(startInfo, ci) {
				break
			}
			dotGit := filepath.Join(cur, ".git")
			if rdg, _, info, ok := sc.walkPath(dotGit, false); ok {
				if !sameOwner(rc, ci) || !sameOwner(rdg, info) {
					return "", false
				} else if info.IsDir() {
					ok := sc.validGitDir(dotGit, true) && !sc.hasWorktreeConfig(dotGit)
					return pick(ok, cur, ""), ok
				} else if info.Mode().IsRegular() {
					if data, err := sc.readSmallFile(dotGit); err != nil {
						sc.unknown = sc.unknown || errors.Is(err, errOpaque)
					} else if target, ok := strings.CutPrefix(strings.TrimSuffix(strings.TrimSuffix(data, "\n"), "\r"), "gitdir: "); ok && target != "" {
						if resolved, ok := sc.resolveJoined(cur, target, false); ok {
							if r, _, gInfo, gFound := sc.walkPath(resolved, true); gFound && gInfo.IsDir() && sc.validGitDir(r, true) && !sc.hasWorktreeConfig(r) {
								return cur, true
							}
						}
					}
				}
				return "", false
			} else if cur == filepath.Dir(cur) || sc.validGitDir(cur, false) {
				break
			}
		}
	}
	return "", false
}
func (sc *scan) gitCommonDir(dir string) (string, bool, bool) {
	if common, err := sc.readSmallFile(filepath.Join(dir, "commondir")); errors.Is(err, fs.ErrNotExist) {
		return "", false, true
	} else if c := strings.TrimSuffix(strings.TrimSuffix(common, "\n"), "\r"); err == nil && c != "" {
		r, ok := sc.resolveJoined(dir, c, false)
		return r, true, ok
	}
	return "", true, false
}
func (sc *scan) validGitDir(dir string, owned bool) bool {
	if rd, _, di, ok := sc.walkPath(dir, true); owned && (!ok || !sameOwner(rd, di)) {
		return false
	}
	head, err := sc.readSmallFile(filepath.Join(dir, "HEAD"))
	h, ref := strings.TrimRight(head, " \t\r\n"), strings.TrimLeft(strings.TrimPrefix(strings.TrimRight(head, " \t\r\n"), "ref:"), " \t")
	if err != nil || (!strings.HasPrefix(h, "ref:") || !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "/.") || strings.Contains(ref, "@{") || strings.Contains(ref, ".lock/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") || strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) })) && (len(h) != 40 && len(h) != 64 || strings.Trim(h, "0123456789abcdefABCDEF") != "") {
		return false
	}
	hasDir := func(p string) bool { _, _, info, ok := sc.walkPath(p, true); return ok && info.IsDir() }
	if hasDir(filepath.Join(dir, "objects")) && (hasDir(filepath.Join(dir, "refs")) || hasDir(filepath.Join(dir, "reftable"))) {
		return true
	} else if r, present, ok := sc.gitCommonDir(dir); present && ok && hasDir(filepath.Join(r, "objects")) {
		rr, _, ri, rOk := sc.walkPath(r, true)
		return !owned || rOk && sameOwner(rr, ri)
	}
	return false
}
func (sc *scan) hasWorktreeConfig(gitDir string) bool {
	r, present, ok := sc.gitCommonDir(gitDir)
	if !ok || slices.ContainsFunc([]string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "ProgramData", "GIT_CONFIG_NOSYSTEM"}, sc.isUnresolvedVar) {
		return true
	}
	paths := append([]string{filepath.Join(gitDir, "config"), filepath.Join(gitDir, "config.worktree")}, pick(present, []string{filepath.Join(r, "config")}, nil)...)
	if !slices.Contains([]string{"1", "true", "yes", "on"}, strings.ToLower(sc.envVal("GIT_CONFIG_NOSYSTEM"))) {
		if gitBin, ok := sc.trustedCmd("git"); !ok {
			return true
		} else if gitBin != "" {
			p2 := filepath.Dir(filepath.Dir(gitBin))
			paths = append(paths, filepath.Join(p2, "etc", "gitconfig"), filepath.Join(filepath.Dir(p2), "etc", "gitconfig"), filepath.Join(p2, "mingw64", "etc", "gitconfig"))
		}
		paths = append(paths, pick(filepath.Separator == '/', []string{"/etc/gitconfig", "/usr/local/etc/gitconfig", "/opt/homebrew/etc/gitconfig", "/Library/Developer/CommandLineTools/usr/etc/gitconfig"}, pick(sc.envVal("ProgramData") != "", []string{filepath.Join(sc.envVal("ProgramData"), "Git", "config")}, nil))...)
	}
	home, hasHome := sc.homeDir()
	paths = append(append(paths, pick(sc.envVal("XDG_CONFIG_HOME") != "", []string{filepath.Join(sc.envVal("XDG_CONFIG_HOME"), "git", "config")}, nil)...), pick(hasHome && home != "", []string{filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config")}, nil)...)
	return slices.ContainsFunc(paths, sc.badGitConfig)
}
func (sc *scan) badGitConfig(p string) bool {
	inSec, curSec, cont, inQ, fmtV1, hasExt := false, "", false, false, false, false
	scanVal := func(v string) (string, bool) {
		out, end := []byte(nil), 0
		cont = false
		for i := 0; i < len(v) && (inQ || cont || v[i] != '#' && v[i] != ';'); i++ {
			if cont && strings.IndexByte(`\"ntb`, v[i]) < 0 {
				return "", true
			} else if !cont && (v[i] == '\\' || v[i] == '"') {
				inQ, cont = pick(v[i] == '"', !inQ, inQ), v[i] == '\\'
			} else if cont || inQ || v[i] != ' ' && v[i] != '\t' || len(out) > 0 {
				out, end, cont = append(out, v[i]), pick(cont || inQ || v[i] != ' ' && v[i] != '\t', len(out)+1, end), false
			}
		}
		return string(out[:end]), !cont && inQ
	}
	return sc.fileHasLine(p, func(l string) bool {
		t := strings.TrimRight(strings.TrimLeft(strings.TrimPrefix(l, "\ufeff"), " \t"), "\r")
		if cont {
			_, bad := scanVal(t)
			return bad
		} else if t == "" || t[0] == '#' || t[0] == ';' {
			return false
		} else if t[0] == '[' {
			if sec, after, hasSub, ok := parseGitSection(t); !ok || strings.HasPrefix(sec, "include") {
				return true
			} else if inSec, curSec, t = true, pick(hasSub, "", sec), strings.TrimLeft(after, " \t"); t == "" || t[0] == '#' || t[0] == ';' {
				return false
			}
		}
		key, raw, hasEq := strings.Cut(t, "=")
		val, bad := scanVal(raw)
		k, lv := strings.ToLower(strings.TrimSpace(key)), strings.ToLower(val)
		fmtV1, hasExt = pick(curSec == "core" && k == "repositoryformatversion" && hasEq && !cont, lv == "1", fmtV1), hasExt || curSec == "extensions"
		return !inSec || k == "" || strings.IndexByte(alnum[:52], k[0]) < 0 || strings.Trim(k, alnum+"-") != "" || bad || badGitKey(curSec, k, lv, hasEq, cont)
	}) || inQ || cont || hasExt && !fmtV1
}
func parseGitSection(t string) (sec, after string, hasSub, ok bool) {
	i := 1
	for ; i < len(t) && strings.IndexByte(alnum+"-.", t[i]) >= 0; i++ {
	}
	sec = strings.ToLower(t[1:i])
	if j := len(t) - len(strings.TrimLeft(t[i:], " \t")); sec != "" && j > i {
		for hasSub, i = true, pick(j < len(t) && t[j] == '"', j+1, len(t)); i < len(t) && t[i] != '"'; i++ {
			i += pick(t[i] == '\\', 1, 0)
		}
		i++
	}
	return sec, t[min(i+1, len(t)):], hasSub, sec != "" && i < len(t) && t[i] == ']'
}
func badGitKey(sec, k, val string, hasEq, cont bool) bool {
	return sec == "core" && (k == "worktree" || k == "bare" && (!hasEq || cont || !slices.Contains([]string{"false", "no", "off", "0"}, val)) || k == "repositoryformatversion" && (!hasEq || cont || val != "0" && val != "1")) || sec == "extensions" && (cont || k == "objectformat" && (!hasEq || val != "sha1" && val != "sha256") || k == "partialclone" && (!hasEq || val == "") || (k == "worktreeconfig" || k == "preciousobjects") && hasEq && !slices.Contains([]string{"true", "false", "yes", "no", "on", "off", "1", "0"}, val) || !slices.Contains([]string{"objectformat", "partialclone", "worktreeconfig", "preciousobjects"}, k))
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
	bad := n == len(buf) || err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)
	return pick(bad, "", string(buf[:n])), pick(bad, errOpaque, nil)
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
				} else if _, _, _, ok := sc.walkPath(filepath.Join(sub, ".git"), false); ok {
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
		if r, _, _, found := sc.walkPath(home, true); (found || !sc.unknown) && samePath(root, pick(found, r, home)) {
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
		cache, hasCache := modcache(sc, nil)
		home, hasHome := sc.homeDir()
		sc.none = append(append(pick(runtime.GOOS == "darwin", []string{sep + "System"}, []string{}), pick(hasCache, []string{cache}, nil)...), pick(runtime.GOOS == "darwin" && hasHome && home != "", []string{filepath.Join(home, "Library")}, nil)...)
	}
	return filepath.Base(p) == "node_modules" || strings.Contains(p, sep+"node_modules"+sep) || slices.ContainsFunc(sc.none, func(root string) bool { return p == root || strings.HasPrefix(p, root+sep) })
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
