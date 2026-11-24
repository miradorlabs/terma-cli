package hookrun

import (
	"cmp"
	"errors"
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
	for i, d := range dirs {
		if p, info := sc.reach(t, d); p != "" {
			if res.named = res.named || info != nil; info != nil && info.IsDir() {
				if i == 0 {
					res.dir = p
				}
				res.folders = append(res.folders, p)
			}
		} else if r.mayCreate(slashed) {
			if p, ok := sc.path(t, d); ok {
				sc.placeParent(p)
			}
		}
	}
	return res
}

const maxSymlinkHops, maxPathComponents, maxPathBytes = 8, 128, 4096

// walkPath traverses p component-by-component via directory-handle-relative os.Root operations,
// preventing TOCTOU parent-symlink swaps to remote shares.
func (sc *scan) walkPath(p string, followFinal bool) (resolved, parent string, info os.FileInfo, ok bool) {
	if !filepath.IsAbs(p) {
		if !filepath.IsAbs(sc.dir) {
			return "", "", nil, false
		}
		p = sc.dir + string(filepath.Separator) + p
	}
	if sc.unknown || remote(p, sc.cwd) || len(p) > maxPathBytes {
		return "", "", nil, !sc.fail()
	}
	vol := filepath.VolumeName(p)
	root, err := os.OpenRoot(vol + string(filepath.Separator))
	if err != nil {
		return "", "", nil, false
	}
	stack, paths := []*os.Root{root}, []string{vol + string(filepath.Separator)}
	defer func() {
		for _, r := range stack {
			_ = r.Close()
		}
	}()
	for parts, hops := strings.FieldsFunc(p[len(vol):], isSep), 0; len(parts) > 0; {
		if len(parts)+len(stack) > maxPathComponents || !sc.spendStat(1) {
			return "", "", nil, !sc.fail()
		}
		part, cur, curPath := parts[0], stack[len(stack)-1], paths[len(paths)-1]
		if parts = parts[1:]; part == "" || part == "." {
			continue
		} else if part == ".." {
			if len(stack) > 1 {
				_ = cur.Close()
				stack, paths = stack[:len(stack)-1], paths[:len(paths)-1]
			}
			continue
		}
		fi, err := cur.Lstat(part)
		if err != nil {
			return filepath.Join(append([]string{curPath, part}, parts...)...), curPath, nil, false
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if !followFinal && len(parts) == 0 {
				return filepath.Join(curPath, part), curPath, fi, true
			}
			target, err := cur.Readlink(part)
			if hops++; hops > maxSymlinkHops || err != nil || target == "" || remote(target, sc.cwd) || strings.HasPrefix(target, `\??\`) || filepath.VolumeName(target) != "" && !filepath.IsAbs(target) || len(curPath)+len(target) > maxPathBytes {
				return "", "", nil, !sc.fail()
			}
			tVol := filepath.VolumeName(target)
			if filepath.IsAbs(target) || os.IsPathSeparator(target[0]) {
				newVol := cmp.Or(tVol, filepath.VolumeName(curPath), filepath.VolumeName(sc.cwd)) + string(filepath.Separator)
				nr, err := os.OpenRoot(newVol)
				if err != nil {
					return "", "", nil, !sc.fail()
				}
				for _, r := range stack {
					_ = r.Close()
				}
				stack, paths = []*os.Root{nr}, []string{newVol}
			}
			parts = append(strings.FieldsFunc(target[len(tVol):], isSep), parts...)
			continue
		}
		nextPath := filepath.Join(curPath, part)
		if len(parts) == 0 {
			return nextPath, curPath, fi, true
		} else if !fi.IsDir() {
			return "", "", nil, false
		} else if next, err := cur.OpenRoot(part); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return "", curPath, nil, false
			}
			return "", "", nil, !sc.fail()
		} else {
			stack, paths = append(stack, next), append(paths, nextPath)
		}
	}
	cur, curPath := stack[len(stack)-1], paths[len(paths)-1]
	fi, err := cur.Lstat(".")
	return curPath, filepath.Dir(curPath), fi, err == nil
}
func (sc *scan) hasRemoteLink(p string) bool {
	if !sc.unknown {
		_, _, _, _ = sc.walkPath(p, true)
	}
	return sc.unknown
}
func (sc *scan) resolveTarget(base, raw string, _ int) (string, bool) {
	if remote(raw, sc.cwd) || filepath.VolumeName(raw) != "" && !filepath.IsAbs(raw) {
		return "", !sc.fail()
	}
	if !filepath.IsAbs(raw) && len(raw) > 0 && os.IsPathSeparator(raw[0]) {
		raw = cmp.Or(filepath.VolumeName(base), filepath.VolumeName(sc.cwd)) + raw
	}
	if !filepath.IsAbs(raw) && base != "" {
		if len(base)+len(raw)+1 > maxPathBytes {
			return "", !sc.fail()
		}
		raw = strings.TrimRight(base, string(filepath.Separator)) + string(filepath.Separator) + raw
	}
	if len(raw) > maxPathBytes {
		return "", !sc.fail()
	} else if !hasDotDot(raw) {
		return filepath.Clean(raw), true
	}
	res, _, _, found := sc.walkPath(raw, true)
	return res, found || !sc.unknown && res != ""
}
func (sc *scan) reach(w, dir string) (string, os.FileInfo) {
	p, ok := sc.path(w, dir)
	if !ok {
		return "", nil
	}
	res, parent, info, found := sc.walkPath(p, false)
	if !found {
		return "", nil
	} else if info.IsDir() {
		sc.place(res)
		return res, info
	} else if sc.place(parent); info.Mode()&fs.ModeSymlink == 0 {
		return res, info
	}
	if target, tParent, tInfo, tFound := sc.walkPath(p, true); tFound {
		if tInfo.IsDir() {
			sc.place(target)
		} else {
			sc.place(tParent)
		}
		return res, tInfo
	} else if !sc.unknown && tParent != "" {
		sc.place(tParent)
	}
	return res, nil
}
func (sc *scan) placeParent(p string) {
	if _, parent, _, found := sc.walkPath(p, true); !found && !sc.unknown && parent != "" {
		sc.place(parent)
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
			sc.unknown = sc.unknown || remote(home, sc.cwd) || sc.isUnresolvedVar("HOME")
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
func urlPath(w string) (string, bool) {
	scheme, rest, ok := strings.Cut(w, "://")
	if !ok || !isScheme(scheme) {
		return "", false
	} else if !strings.EqualFold(scheme, "file") {
		return "", true
	} else if len(rest) > 2 && rest[0] == '/' && rest[2] == ':' {
		return rest[1:], true
	} else if strings.HasPrefix(rest, "/") {
		return rest, true
	}
	return "//" + rest, true
}
func isScheme(s string) bool {
	return len(s) > 1 && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') && !strings.ContainsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '+' && r != '-' && r != '.'
	})
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
	if name, v, ok := strings.Cut(w, "="); ok && (strings.HasPrefix(w, "-") || isName(name)) {
		return v
	} else if len(w) > 2 && w[0] == '-' && w[1] != '-' {
		return w[2:]
	} else if strings.HasPrefix(w, "-") {
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
