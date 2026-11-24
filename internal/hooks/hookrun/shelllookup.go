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
	if filepath.Separator == '/' && strings.HasPrefix(t, "///") && strings.TrimLeft(t, "/") != "" {
		t = "/" + strings.TrimLeft(t, "/")
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
	if sc.reads != nil && res.named && (r == plain || r == folder || r == part) {
		sc.reads.named, sc.reads.folders = true, append(sc.reads.folders, res.folders...)
	}
	if r == target && res.dir != "" {
		sc.prev, sc.dir = sc.dir, res.dir
	}
}
func (sc *scan) resolve(t string, r role, slashed bool) cached {
	var res cached
	record := func(dir string, primary bool) string {
		p, info := sc.reach(t, dir)
		if info != nil {
			res.named = true
			if info.IsDir() {
				if primary {
					res.dir = p
				}
				res.folders = append(res.folders, p)
			}
		}
		return p
	}
	p, back := record(sc.dir, true), ""
	if sc.dir != sc.cwd {
		back = record(sc.cwd, false)
	}
	if p == "" && back == "" && r.mayCreate(slashed) {
		if sc.reachNew(t, sc.dir); sc.dir != sc.cwd {
			sc.reachNew(t, sc.cwd)
		}
	}
	return res
}

const maxSymlinkHops, maxPathComponents, maxPathBytes = 8, 128, 4096

// hasRemoteLink walks components of p top-down without following symlinks, failing closed on remote shares or >maxSymlinkHops.
func (sc *scan) hasRemoteLink(p string) bool {
	if sc.unknown {
		return true
	}
	clean := filepath.Clean(p)
	if sc.cwd != "" && clean == filepath.Clean(sc.cwd) {
		if !sc.cwdChecked {
			sc.cwdChecked, sc.cwdRemote = true, sc.hasRemoteLinkAt(clean, 0)
		}
		return sc.cwdRemote
	}
	return sc.hasRemoteLinkAt(clean, 0)
}
func (sc *scan) hasRemoteLinkAt(p string, hops int) bool {
	if remote(p, sc.cwd) || len(p) > maxPathBytes {
		sc.unknown = true
		return true
	}
	var chain []string
	for cur := p; ; cur = filepath.Dir(cur) {
		if chain = append(chain, cur); len(chain) > maxPathComponents {
			sc.unknown = true
			return true
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !sc.spendStat(1) {
			return true
		}
		if info, err := os.Lstat(chain[i]); err != nil {
			return false
		} else if info.Mode()&fs.ModeSymlink != 0 {
			if _, bad := sc.resolveLink(chain[i], hops); bad {
				return true
			}
		}
	}
	return false
}
func (sc *scan) resolveTarget(base, raw string, hops int) (string, bool) {
	if remote(raw, sc.cwd) {
		sc.unknown = true
		return "", false
	}
	if hasDotDot(raw) {
		if filepath.IsAbs(raw) {
			vol := filepath.VolumeName(raw)
			return sc.cleanSafeAt(vol+string(filepath.Separator), raw[len(vol):], hops)
		}
		return sc.cleanSafeAt(base, raw, hops)
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(base, raw)
	}
	return filepath.Clean(raw), true
}

// resolveLink chases up to maxSymlinkHops hops via Readlink without statting targets,
// checking target ancestors and ".." segments and failing closed on remote targets or hop exhaustion.
func (sc *scan) resolveLink(p string, hops int) (string, bool) {
	cur := p
	for hops < maxSymlinkHops {
		if !sc.spend(1) {
			return "", true
		}
		raw, err := os.Readlink(cur)
		if err != nil {
			return cur, false
		}
		next, ok := sc.resolveTarget(filepath.Dir(cur), raw, hops+1)
		if !ok {
			return "", true
		}
		if cur, hops = next, hops+1; filepath.Dir(cur) != cur && sc.hasRemoteLinkAt(filepath.Dir(cur), hops) {
			return "", true
		}
	}
	if _, err := os.Readlink(cur); err == nil {
		sc.unknown = true
		return "", true
	}
	return cur, false
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
	switch {
	case info.IsDir():
		sc.place(p)
	case info.Mode()&fs.ModeSymlink != 0:
		sc.place(filepath.Dir(p))
		target, bad := sc.resolveLink(p, 0)
		if bad {
			return p, nil
		}
		if resolved, err := filepath.EvalSymlinks(p); err != nil {
			sc.placeParent(target)
		} else if remote(resolved, sc.cwd) {
			sc.unknown = true
			return p, nil
		} else if t, err := os.Stat(resolved); err == nil && t.IsDir() {
			sc.place(resolved)
		} else {
			sc.place(filepath.Dir(resolved))
		}
		if info, err = os.Stat(p); err != nil {
			return p, nil
		}
	default:
		sc.place(filepath.Dir(p))
	}
	return p, info
}
func (sc *scan) placeParent(p string) {
	if len(p) > maxPathBytes || sc.hasRemoteLink(filepath.Dir(p)) {
		return
	}
	for d, ok := filepath.Dir(p), true; ok && sc.spend(1); d = filepath.Dir(d) {
		if info, err := os.Stat(d); err == nil {
			if info.IsDir() {
				if r, err := filepath.EvalSymlinks(d); err == nil && !remote(r, sc.cwd) {
					d = r
				}
				sc.place(d)
			}
			return
		}
		ok = filepath.Dir(d) != d
	}
}
func (sc *scan) reachNew(w, dir string) {
	if p, ok := sc.path(w, dir); ok {
		sc.placeParent(p)
	}
}
func (sc *scan) away(w string, r role) bool {
	if !remote(w, sc.cwd) {
		return false
	}
	isPath := share(w) != "" || len(w) > 2 && !strings.ContainsAny(w, " \t\n") && strings.TrimLeft(w, `/\`) != ""
	sc.unknown = sc.unknown || isPath && r.mustResolve(true)
	return true
}
func (sc *scan) path(w, dir string) (string, bool) {
	base, rel := dir, w
	switch {
	case w == "~" || strings.HasPrefix(w, "~/"):
		home, ok := sc.lookup("HOME")
		if !ok || home == "" || remote(home, sc.cwd) {
			sc.unknown = sc.unknown || remote(home, sc.cwd)
			return "", false
		}
		base, rel = home, strings.TrimLeft(w[1:], "/")
	case remote(w, sc.cwd) || !filepath.IsAbs(w) && dir == "":
		return "", false
	}
	return sc.resolveTarget(base, rel, 0)
}
func hasDotDot(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/..") || os.IsPathSeparator('\\') && (strings.HasPrefix(p, `..\`) || strings.Contains(p, `\..`))
}

// cleanSafeAt resolves symlinks and checks remote shares before collapsing any ".." segment.
func (sc *scan) cleanSafeAt(base, rel string, hops int) (string, bool) {
	if len(base)+len(rel) > maxPathBytes {
		sc.unknown = sc.unknown || hops > 0
		return "", false
	}
	cur := filepath.Clean(base)
	for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) }) {
		if part == "" || part == "." {
			continue
		}
		if part != ".." {
			cur = filepath.Join(cur, part)
			continue
		}
		if sc.hasRemoteLinkAt(cur, hops) || !sc.spend(1) {
			return "", false
		}
		if info, err := os.Lstat(cur); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil || remote(resolved, sc.cwd) {
				sc.unknown = true
				return "", false
			}
			cur = resolved
		}
		cur = filepath.Dir(cur)
	}
	return filepath.Clean(cur), true
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
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (i == 0 || (r < '0' || r > '9') && r != '+' && r != '-' && r != '.') {
			return false
		}
	}
	return len(s) > 1
}

// remote reports whether p starts with two separators (UNC/device path) outside cwd's share.
func remote(p, cwd string) bool {
	s := share(p)
	return len(p) >= 2 && os.IsPathSeparator(p[0]) && os.IsPathSeparator(p[1]) && (s == "" || !strings.EqualFold(s, share(cwd)))
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

func cutSeparator(p string) (string, string, bool) {
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
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return s != ""
}
func pathLike(w string) bool {
	return strings.ContainsRune(w, '/') || strings.ContainsRune(w, filepath.Separator) || filepath.IsAbs(w)
}
