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
func (sc *scan) resolve(t string, r role, slashed bool) (res cached) {
	found := false
	for i, d := range []string{sc.dir, sc.cwd} {
		if i > 0 && d == sc.dir {
			break
		}
		if p, info := sc.reach(t, d); p != "" {
			if found = true; info != nil {
				if res.named = true; info.IsDir() {
					if i == 0 {
						res.dir = p
					}
					res.folders = append(res.folders, p)
				}
			}
		}
	}
	if !found && r.mayCreate(slashed) {
		for i, d := range []string{sc.dir, sc.cwd} {
			if p, ok := sc.path(t, d); (i == 0 || d != sc.dir) && ok {
				sc.placeParent(p)
			}
		}
	}
	return res
}

const maxSymlinkHops, maxPathComponents, maxPathBytes = 8, 128, 4096

// hasRemoteLink walks components of p top-down without following symlinks, failing closed on remote shares or >maxSymlinkHops.
func (sc *scan) hasRemoteLink(p string) bool {
	return sc.unknown || sc.hasRemoteLinkAt(filepath.Clean(p), 0)
}
func (sc *scan) hasRemoteLinkAt(p string, hops int) bool {
	if remote(p, sc.cwd) || len(p) > maxPathBytes {
		return sc.fail()
	}
	var chain []string
	for cur := p; ; cur = filepath.Dir(cur) {
		if chain = append(chain, cur); len(chain) > maxPathComponents {
			return sc.fail()
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
	if remote(raw, sc.cwd) || filepath.VolumeName(raw) != "" && !filepath.IsAbs(raw) {
		return "", !sc.fail()
	}
	if !filepath.IsAbs(raw) && len(raw) > 0 && os.IsPathSeparator(raw[0]) {
		raw = cmp.Or(filepath.VolumeName(base), filepath.VolumeName(sc.cwd)) + raw
	}
	if hasDotDot(raw) {
		if filepath.IsAbs(raw) {
			vol := filepath.VolumeName(raw)
			base, raw = vol+string(filepath.Separator), raw[len(vol):]
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
	for ; hops < maxSymlinkHops; hops++ {
		if !sc.spend(1) {
			return "", true
		}
		raw, err := os.Readlink(cur)
		if err != nil {
			return cur, false
		}
		next, ok := sc.resolveTarget(filepath.Dir(cur), raw, hops+1)
		if !ok || filepath.Dir(next) != next && sc.hasRemoteLinkAt(filepath.Dir(next), hops+1) {
			return "", true
		}
		cur = next
	}
	if _, err := os.Readlink(cur); err == nil {
		return "", sc.fail()
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
		if target, bad := sc.resolveLink(p, 0); bad {
			return p, nil
		} else if resolved, err := filepath.EvalSymlinks(p); err != nil {
			sc.placeParent(target)
		} else if remote(resolved, sc.cwd) {
			sc.fail()
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
	for _, part := range strings.FieldsFunc(rel, isSep) {
		switch part {
		case "", ".":
		case "..":
			if sc.hasRemoteLinkAt(cur, hops) || !sc.spend(1) {
				return "", false
			}
			if info, err := os.Lstat(cur); err == nil && info.Mode()&fs.ModeSymlink != 0 {
				resolved, err := filepath.EvalSymlinks(cur)
				if err != nil || remote(resolved, sc.cwd) {
					return "", !sc.fail()
				}
				cur = resolved
			}
			cur = filepath.Dir(cur)
		default:
			cur = filepath.Join(cur, part)
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
	if host == "" || name == "" || strings.Trim(host, hostBytes) != "" {
		return ""
	}
	return "//" + host + "/" + name
}

const hostBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.$"

func isSep(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) }
func cutSep(p string) (string, string) {
	if i := strings.IndexFunc(p, isSep); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}
func shellValue(w string) string {
	if name, v, ok := strings.Cut(w, "="); ok && (strings.HasPrefix(w, "-") || isName(name)) {
		return v
	}
	if strings.HasPrefix(w, "-") {
		if len(w) > 2 && w[1] != '-' {
			return w[2:]
		}
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
