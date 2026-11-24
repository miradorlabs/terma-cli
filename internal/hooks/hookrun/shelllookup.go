package hookrun

import (
	"cmp"
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
	if !seen && !sc.spend(1) {
		return
	} else if !seen {
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
func (sc *scan) resolve(t string, r role, slashed bool) (res cached) {
	dirs := []string{sc.dir}
	if sc.cwd != sc.dir {
		dirs = append(dirs, sc.cwd)
	}
	found := false
	for i, d := range dirs {
		if p, info := sc.reach(t, d); p != "" {
			if found = true; info != nil && info.IsDir() {
				if i == 0 {
					res.dir = p
				}
				res.folders = append(res.folders, p)
			}
			res.named = res.named || info != nil
		}
	}
	if !found && r.mayCreate(slashed) {
		for _, d := range dirs {
			if p, ok := sc.path(t, d); ok {
				sc.placeParent(p)
			}
		}
	}
	return res
}

const maxSymlinkHops, maxPathComponents, maxPathBytes = 8, 128, 4096

// hasRemoteLink walks components of p top-down without following symlinks, failing closed on remote shares or >maxSymlinkHops.
func (sc *scan) hasRemoteLink(p string) bool {
	if sc.unknown {
		return true
	} else if hasDotDot(p) {
		p, ok := sc.resolveTarget(sc.dir, p, 0)
		return !ok || sc.hasRemoteLinkAt(filepath.Clean(p), 0)
	}
	return sc.hasRemoteLinkAt(filepath.Clean(p), 0)
}
func (sc *scan) hasRemoteLinkAt(p string, hops int) bool {
	if remote(p, sc.cwd) || len(p) > maxPathBytes {
		return sc.fail()
	}
	var chain []string
	for cur := p; ; cur = filepath.Dir(cur) {
		if chain = append(chain, cur); len(chain) > maxPathComponents {
			return sc.fail()
		} else if filepath.Dir(cur) == cur {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !sc.spendStat(1) {
			return true
		} else if info, err := os.Lstat(chain[i]); err != nil {
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
	if remote(raw, sc.cwd) || filepath.VolumeName(raw) != "" && !filepath.IsAbs(raw) {
		return "", !sc.fail()
	}
	if !filepath.IsAbs(raw) && len(raw) > 0 && os.IsPathSeparator(raw[0]) {
		raw = cmp.Or(filepath.VolumeName(base), filepath.VolumeName(sc.cwd)) + raw
	}
	if hasDotDot(raw) {
		if vol := filepath.VolumeName(raw); filepath.IsAbs(raw) {
			base, raw = vol+string(filepath.Separator), raw[len(vol):]
		}
		return sc.cleanSafeAt(base, raw, hops)
	} else if !filepath.IsAbs(raw) {
		raw = filepath.Join(base, raw)
	}
	return filepath.Clean(raw), true
}

// resolveLink chases up to maxSymlinkHops hops via Readlink, checking ancestors and failing closed on remote targets.
func (sc *scan) resolveLink(p string, hops int) (string, bool) {
	for ; hops < maxSymlinkHops; hops++ {
		if !sc.spend(1) {
			return "", true
		} else if raw, err := os.Readlink(p); err != nil {
			return p, false
		} else if next, ok := sc.resolveTarget(filepath.Dir(p), raw, hops+1); !ok || filepath.Dir(next) != next && sc.hasRemoteLinkAt(filepath.Dir(next), hops+1) {
			return "", true
		} else {
			p = next
		}
	}
	_, err := os.Readlink(p)
	return p, err == nil && sc.fail()
}
func (sc *scan) reach(w, dir string) (string, os.FileInfo) {
	p, ok := sc.path(w, dir)
	if !ok || len(p) > maxPathBytes || sc.hasRemoteLink(filepath.Dir(p)) {
		return "", nil
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", nil
	} else if info.IsDir() {
		sc.place(p)
		return p, info
	} else if sc.place(filepath.Dir(p)); info.Mode()&fs.ModeSymlink == 0 {
		return p, info
	}
	if target, bad := sc.resolveLink(p, 0); bad {
		return p, nil
	} else if resolved, err := filepath.EvalSymlinks(p); err != nil {
		sc.placeParent(target)
		return p, nil
	} else if remote(resolved, sc.cwd) && sc.fail() {
		return p, nil
	} else if info, err = os.Stat(resolved); err == nil && info.IsDir() {
		sc.place(resolved)
	} else {
		sc.place(filepath.Dir(resolved))
	}
	return p, info
}
func (sc *scan) placeParent(p string) {
	if len(p) > maxPathBytes || sc.hasRemoteLink(filepath.Dir(p)) {
		return
	}
	for d, ok := filepath.Dir(p), true; ok && sc.spend(1); d, ok = filepath.Dir(d), filepath.Dir(d) != d {
		if info, err := os.Stat(d); err == nil {
			if info.IsDir() {
				if r, err := filepath.EvalSymlinks(d); err == nil && !remote(r, sc.cwd) {
					d = r
				}
				sc.place(d)
			}
			return
		}
	}
}
func (sc *scan) away(w string, r role) bool {
	if !remote(w, sc.cwd) {
		return false
	}
	sc.unknown = sc.unknown || (share(w) != "" || len(w) > 2 && !strings.ContainsAny(w, " \t\n") && strings.TrimLeft(w, `/\`) != "") && r.mustResolve(true)
	return true
}
func (sc *scan) path(w, dir string) (string, bool) {
	if w == "~" || strings.HasPrefix(w, "~/") {
		home, ok := sc.lookup("HOME")
		if !ok || home == "" || remote(home, sc.cwd) {
			sc.unknown = sc.unknown || remote(home, sc.cwd)
			return "", false
		}
		return sc.resolveTarget(home, strings.TrimLeft(w[1:], "/"), 0)
	} else if remote(w, sc.cwd) || !filepath.IsAbs(w) && dir == "" {
		return "", false
	}
	return sc.resolveTarget(dir, w, 0)
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
	for _, part := range strings.FieldsFunc(rel, isSep) {
		if part == "" || part == "." {
			continue
		} else if part != ".." {
			cur = filepath.Join(cur, part)
		} else if sc.hasRemoteLinkAt(cur, hops) || !sc.spend(1) {
			return "", false
		} else {
			if info, err := os.Lstat(cur); err == nil && info.Mode()&fs.ModeSymlink != 0 {
				resolved, err := filepath.EvalSymlinks(cur)
				if err != nil || remote(resolved, sc.cwd) {
					return "", !sc.fail()
				}
				cur = resolved
			}
			cur = filepath.Dir(cur)
		}
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
	host, rest := cutSep(p[2:])
	name, _ := cutSep(rest)
	if host == "" || name == "" || strings.Trim(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.$") != "" {
		return ""
	}
	return "//" + host + "/" + name
}
func isSep(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) }
func cutSep(p string) (string, string) {
	if i := strings.IndexFunc(p, isSep); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}
func shellValue(w string) string {
	switch name, v, ok := strings.Cut(w, "="); {
	case ok && (strings.HasPrefix(w, "-") || isName(name)):
		return v
	case len(w) > 2 && w[0] == '-' && w[1] != '-':
		return w[2:]
	case strings.HasPrefix(w, "-"):
		return ""
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
