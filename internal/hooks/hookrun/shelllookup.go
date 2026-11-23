package hookrun

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// How ShellPaths looks at a word: what it names from where the command is and from the cwd,
// a symlink's target included, or the nearest folder above a file it may create; and what no
// lookup touches: a URL's host and path, and a path on another machine (remote).

// candidate looks at t, a word as expanded, in role r (resolve), once from each place in
// each role, replays any folder operands it matched onto the current command's sc.reads
// (on both cache misses and cache hits), and then moves where the command is when t is a
// cd's target that names a folder. A URL names no path here but a file: URL's (urlPath).
func (sc *scan) candidate(t string, r role) {
	if !r.whole() {
		t = shellValue(t)
	}
	if p, isURL := urlPath(t); isURL {
		t = p
	}
	slashed := pathLike(t)
	if t == "" || !slashed && r.needsSlash() || sc.away(t, r) {
		return
	}
	c := candidate{t, sc.dir, r}
	res, seen := sc.examined[c]
	if !seen {
		if !sc.spend(1) {
			return
		}
		res = sc.resolve(t, r, slashed)
		sc.examined[c] = res
	}
	sc.recordCached(res, r)
	if r == target && res.dir != "" {
		sc.prev, sc.dir = sc.dir, res.dir
	}
}

// resolve reaches what t, a word in role r, names from where the command is and from the
// cwd, as an earlier cd may have been in a branch not taken or a subshell, and returns the
// cached directory and operand summary without mutating sc.reads. A word that names nothing
// reaches nothing, but one that may be a new file reaches its nearest existing folder.
func (sc *scan) resolve(t string, r role, slashed bool) cached {
	p, info := sc.reach(t, sc.dir)
	var back string
	var backInfo os.FileInfo
	if sc.dir != sc.cwd {
		back, backInfo = sc.reach(t, sc.cwd)
	}
	var res cached
	if info != nil {
		res.named = true
		if info.IsDir() {
			res.dir = p
			res.folders = append(res.folders, p)
		}
	}
	if backInfo != nil {
		res.named = true
		if backInfo.IsDir() {
			res.folders = append(res.folders, back)
		}
	}
	if p == "" && back == "" && r.mayCreate(slashed) {
		sc.reachNew(t, sc.dir)
		if sc.dir != sc.cwd {
			sc.reachNew(t, sc.cwd)
		}
	}
	return res
}

// recordCached replays the operands recorded in res onto the current command's sc.reads
// when r is an operand role, so a later program naming the same folder as an earlier one
// (for example `ls .. && rg -n TODO ..`) still walks below it.
func (sc *scan) recordCached(res cached, r role) {
	if sc.reads == nil || !res.named || r != plain && r != folder && r != part {
		return
	}
	sc.reads.named = true
	sc.reads.folders = append(sc.reads.folders, res.folders...)
}

// linkTargetRemote follows up to 8 hops of symlink p with Readlink (without statting
// targets) and reports whether any hop points at a remote share, setting sc.unknown if so.
func (sc *scan) linkTargetRemote(p string) bool {
	cur := p
	for range 8 {
		raw, err := os.Readlink(cur)
		if err != nil {
			return false
		}
		if remote(raw, sc.cwd) {
			if share(raw) != "" {
				sc.unknown = true
			}
			return true
		}
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(filepath.Dir(cur), raw)
		}
		cur = raw
	}
	return false
}

// reachEntry adds the place of p, which exists and Lstat described as info: a folder is its
// own, anything else its folder's, and a symlink's target's place as well.
func (sc *scan) reachEntry(p string, info fs.FileInfo) {
	switch {
	case info.IsDir():
		sc.place(p)
	case info.Mode()&fs.ModeSymlink != 0:
		sc.place(filepath.Dir(p))
		if sc.linkTargetRemote(p) {
			return
		}
		if target, err := filepath.EvalSymlinks(p); err == nil && !remote(target, sc.cwd) {
			if t, err := os.Stat(target); err == nil && t.IsDir() {
				sc.place(target)
			} else {
				sc.place(filepath.Dir(target))
			}
		}
	default:
		sc.place(filepath.Dir(p))
	}
}

// reach adds the place of the path w names from dir when something is there, and returns
// the path with what is there, a symlink followed.
func (sc *scan) reach(w, dir string) (string, os.FileInfo) {
	p, ok := sc.path(w, dir)
	if !ok {
		return "", nil
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", nil
	}
	sc.reachEntry(p, info)
	if info.Mode()&fs.ModeSymlink != 0 {
		if sc.linkTargetRemote(p) {
			return p, nil
		}
		if info, err = os.Stat(p); err != nil {
			return p, nil
		}
	}
	return p, info
}

// maxPathBytes caps the length of a path reachNew walks up, so a long non-path token with
// slashes (such as a base64 payload) cannot cause quadratic filepath.Dir + stat work.
const maxPathBytes = 4096

// reachNew adds the nearest existing folder above the path w names from dir, where a file
// the command creates would go; none when the nearest existing path is not a folder.
func (sc *scan) reachNew(w, dir string) {
	p, ok := sc.path(w, dir)
	if !ok || len(p) > maxPathBytes {
		return
	}
	for d := filepath.Dir(p); ok; d = filepath.Dir(d) {
		if !sc.spend(1) {
			return
		}
		if info, err := os.Stat(d); err == nil {
			if info.IsDir() {
				sc.place(d)
			}
			return
		}
		ok = filepath.Dir(d) != d
	}
}

// away reports whether the path the word w, in role r, names is one no lookup may touch
// (remote): a share's is a path not resolved, unknown when r must resolve one, and anything
// else so written (// TODO) names none.
func (sc *scan) away(w string, r role) bool {
	if !remote(w, sc.cwd) {
		return false
	}
	sc.unknown = sc.unknown || share(w) != "" && r.mustResolve(true)
	return true
}

// path is the path the word w names from dir: ~ is the home directory. None when no lookup
// may touch it (remote).
func (sc *scan) path(w, dir string) (string, bool) {
	switch {
	case w == "~" || strings.HasPrefix(w, "~/"):
		home, ok := sc.lookup("HOME")
		return filepath.Join(home, w[1:]), ok && home != "" && !remote(home, sc.cwd)
	case remote(w, sc.cwd):
		return "", false
	case filepath.IsAbs(w):
		return filepath.Clean(w), true
	}
	return filepath.Join(dir, w), dir != ""
}

// urlPath reports whether w is a URL (scheme://...), and the path it names on this machine: a
// file: URL's (file:///x is /x, file:///C:/x C:/x, file://host/x the share's //host/x), none
// for any other's.
func urlPath(w string) (string, bool) {
	scheme, rest, ok := strings.Cut(w, "://")
	switch {
	case !ok || !isScheme(scheme):
		return "", false
	case !strings.EqualFold(scheme, "file"):
		return "", true
	case len(rest) > 2 && rest[0] == '/' && rest[2] == ':':
		return rest[1:], true
	case strings.HasPrefix(rest, "/"):
		return rest, true
	}
	return "//" + rest, true
}

// isScheme reports whether s is a URL's scheme: a letter, then letters, digits, +, - and .,
// two or more in all (C: is a drive).
func isScheme(s string) bool {
	for i, r := range s {
		letter := 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || (r < '0' || r > '9') && r != '+' && r != '-' && r != '.') {
			return false
		}
	}
	return len(s) > 1
}

// remote reports whether no lookup may touch the path p, as it starts with two separators,
// on any system: Windows takes it for another machine's (//host/share, \\host\share) or a
// device's (\\?\), and a lookup of one may wait on the network far past a hook's deadline.
// One on the share cwd is on is not: the hook reads that share anyway.
func remote(p, cwd string) bool {
	if len(p) < 2 || !os.IsPathSeparator(p[0]) || !os.IsPathSeparator(p[1]) {
		return false
	}
	s := share(p)
	return s == "" || !strings.EqualFold(s, share(cwd))
}

// share is the share on another machine that the path p is on, //host/share, its host a name
// a machine may have (letters, digits, -, _, . and $: \\wsl$\Ubuntu) and its share a folder
// there; "" for any other path, // TODO and \\?\C:\ included.
func share(p string) string {
	if len(p) < 2 || !os.IsPathSeparator(p[0]) || !os.IsPathSeparator(p[1]) {
		return ""
	}
	host, rest, _ := cutSeparator(p[2:])
	name, _, _ := cutSeparator(rest)
	if host == "" || name == "" || strings.Trim(host, hostBytes) != "" {
		return ""
	}
	return "//" + host + "/" + name
}

// hostBytes are those of a name a machine may have.
const hostBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.$"

// cutSeparator is p cut at its first separator.
func cutSeparator(p string) (before, after string, found bool) {
	if i := strings.IndexFunc(p, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) }); i >= 0 {
		return p[:i], p[i+1:], true
	}
	return p, "", false
}

// shellValue is the part of the shell word w that may be a path: an option's value after
// its =, or a one-letter option's value written against it (-o/path), an assignment's
// value (NAME=/path), else w itself. An option with no value has none.
func shellValue(w string) string {
	if strings.HasPrefix(w, "-") {
		if _, v, ok := strings.Cut(w, "="); ok {
			return v
		}
		if len(w) > 2 && w[1] != '-' {
			return w[2:]
		}
		return ""
	}
	if name, v, ok := strings.Cut(w, "="); ok && isName(name) {
		return v
	}
	return w
}

// isName reports whether s is a shell variable's name: letters, digits and underscores, not
// starting with a digit.
func isName(s string) bool {
	for i, r := range s {
		letter := r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return s != ""
}

// pathLike reports whether a shell word may name a path: it holds a slash, or on Windows a
// backslash, or is absolute (C:\x). A quoted Windows path keeps its backslashes.
func pathLike(w string) bool {
	return strings.ContainsRune(w, '/') || strings.ContainsRune(w, filepath.Separator) || filepath.IsAbs(w)
}
