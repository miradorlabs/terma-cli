package hookrun

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

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
	keyDir := sc.dir
	if t == "~" || strings.HasPrefix(t, "~/") {
		home, _ := sc.lookup("HOME")
		keyDir = "\x00" + home
	}
	c := candidate{t, keyDir, r}
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

func (sc *scan) recordCached(res cached, r role) {
	if sc.reads == nil || !res.named || r != plain && r != folder && r != part {
		return
	}
	sc.reads.named = true
	sc.reads.folders = append(sc.reads.folders, res.folders...)
}

const maxSymlinkHops = 8

// hasRemoteLink walks components of p top-down without following symlinks, failing closed if
// any component traverses a remote share or > maxSymlinkHops hops.
func (sc *scan) hasRemoteLink(p string) bool { return sc.hasRemoteLinkAt(filepath.Clean(p), 0) }

func (sc *scan) hasRemoteLinkAt(p string, hops int) bool {
	if remote(p, sc.cwd) {
		sc.unknown = true
		return true
	}
	if len(p) > maxPathBytes {
		return false
	}
	var chain []string
	for cur := p; ; {
		chain = append(chain, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(chain) - 1; i >= 0; i-- {
		cur := chain[i]
		if !sc.spend(1) {
			return true
		}
		info, err := os.Lstat(cur)
		if err != nil {
			return false
		}
		if info.Mode()&fs.ModeSymlink != 0 && sc.linkTargetRemote(cur, hops) {
			return true
		}
	}
	return false
}

// linkTargetRemote follows up to maxSymlinkHops hops via Readlink (without statting targets),
// checking target ancestors and failing closed on remote targets or hop-cap exhaustion.
func (sc *scan) linkTargetRemote(p string, hops int) bool {
	cur := p
	for hops < maxSymlinkHops {
		if !sc.spend(1) {
			return true
		}
		raw, err := os.Readlink(cur)
		if err != nil {
			return false
		}
		hops++
		if remote(raw, sc.cwd) {
			sc.unknown = true
			return true
		}
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(filepath.Dir(cur), raw)
		}
		cur = filepath.Clean(raw)
		if dir := filepath.Dir(cur); dir != cur && sc.hasRemoteLinkAt(dir, hops) {
			return true
		}
	}
	if _, err := os.Readlink(cur); err == nil {
		sc.unknown = true
		return true
	}
	return false
}

func (sc *scan) reachEntry(p string, info fs.FileInfo) bool {
	switch {
	case info.IsDir():
		sc.place(p)
	case info.Mode()&fs.ModeSymlink != 0:
		sc.place(filepath.Dir(p))
		if sc.linkTargetRemote(p, 0) {
			return true
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
	return false
}

func (sc *scan) reach(w, dir string) (string, os.FileInfo) {
	p, ok := sc.path(w, dir)
	if !ok || len(p) > maxPathBytes || sc.hasRemoteLink(filepath.Dir(p)) {
		return "", nil
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", nil
	}
	if sc.reachEntry(p, info) {
		return p, nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		if info, err = os.Stat(p); err != nil {
			return p, nil
		}
	}
	return p, info
}

const maxPathBytes = 4096

func (sc *scan) reachNew(w, dir string) {
	p, ok := sc.path(w, dir)
	if !ok || len(p) > maxPathBytes || sc.hasRemoteLink(filepath.Dir(p)) {
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

func (sc *scan) away(w string, r role) bool {
	if !remote(w, sc.cwd) {
		return false
	}
	sc.unknown = sc.unknown || share(w) != "" && r.mustResolve(true)
	return true
}

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

func isScheme(s string) bool {
	for i, r := range s {
		letter := 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || (r < '0' || r > '9') && r != '+' && r != '-' && r != '.') {
			return false
		}
	}
	return len(s) > 1
}

// remote reports whether p starts with two separators (UNC/device path) outside cwd's share.
func remote(p, cwd string) bool {
	if len(p) < 2 || !os.IsPathSeparator(p[0]) || !os.IsPathSeparator(p[1]) {
		return false
	}
	s := share(p)
	return s == "" || !strings.EqualFold(s, share(cwd))
}

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

const hostBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.$"

func cutSeparator(p string) (before, after string, found bool) {
	if i := strings.IndexFunc(p, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) }); i >= 0 {
		return p[:i], p[i+1:], true
	}
	return p, "", false
}

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

func isName(s string) bool {
	for i, r := range s {
		letter := r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return s != ""
}

func pathLike(w string) bool {
	return strings.ContainsRune(w, '/') || strings.ContainsRune(w, filepath.Separator) || filepath.IsAbs(w)
}
