package hookrun

import (
	"cmp"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (sc *scan) candidate(t string, r role) {
	if !r.whole() {
		t = shellValue(t)
	}
	if p, isURL, bad := urlPath(t); bad && sc.fail() {
		return
	} else if isURL {
		t = p
	}
	if filepath.Separator == '/' && strings.HasPrefix(t, "///") && strings.TrimLeft(t, "/") != "" {
		t = "/" + strings.TrimLeft(t, "/")
	}
	slashed, keyDir := pathLike(t), sc.dir
	if t == "" || !slashed && r.needsSlash() || sc.away(t, r) {
		return
	}
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
	for i, d := range []string{sc.dir, sc.cwd} {
		if i == 1 && d == sc.dir {
			break
		}
		if p, info := sc.reach(t, d); p != "" {
			if res.named = res.named || info != nil; info != nil && info.IsDir() {
				if i == 0 {
					res.dir = p
				}
				res.folders = append(res.folders, p)
			}
		} else if r.mayCreate(slashed) {
			if p, ok := sc.path(t, d); ok {
				if _, parent, _, found := sc.walkPath(p, true); !found && !sc.unknown && parent != "" {
					sc.place(parent)
				}
			}
		}
	}
	return res
}

const maxSymlinkHops, maxPathComponents, maxPathBytes = 8, 128, 4096

// walk traverses p component-by-component via dirHandle (openat O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK on Unix,
// os.Root on Windows), preventing TOCTOU symlink swaps and FIFO hangs. openMode: 0=stat, 1=openFile, 2=openDir.
func (sc *scan) walk(p string, followFinal bool, openMode uint8) (resolved, parent string, info os.FileInfo, f *os.File, ok bool) {
	if !filepath.IsAbs(p) && !filepath.IsAbs(sc.dir) {
		return "", "", nil, nil, false
	} else if !filepath.IsAbs(p) {
		p = sc.dir + string(filepath.Separator) + p
	}
	if sc.unknown || remote(p, sc.cwd) || len(p) > maxPathBytes {
		return "", "", nil, nil, !sc.fail()
	}
	vol := filepath.VolumeName(p)
	root, err := openRootDir(vol + string(filepath.Separator))
	if err != nil {
		return "", "", nil, nil, false
	}
	stack, paths := []dirHandle{root}, []string{vol + string(filepath.Separator)}
	closeAll := func() {
		for _, h := range stack {
			h.close()
		}
	}
	defer closeAll()
	for parts, hops := strings.FieldsFunc(p[len(vol):], isSep), 0; len(parts) > 0; {
		if len(parts)+len(stack) > maxPathComponents || !sc.spendStat(1) {
			return "", "", nil, nil, !sc.fail()
		}
		part, cur, curPath := parts[0], stack[len(stack)-1], paths[len(paths)-1]
		if parts = parts[1:]; part == "" || part == "." {
			continue
		} else if part == ".." {
			if len(stack) > 1 {
				cur.close()
				stack, paths = stack[:len(stack)-1], paths[:len(paths)-1]
			}
			continue
		}
		fi, err := cur.lstat(part)
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
			return "", "", nil, nil, !sc.fail()
		} else if err != nil && slices.Contains(parts, "..") {
			return "", "", nil, nil, false
		} else if err != nil {
			return filepath.Join(append([]string{curPath, part}, parts...)...), curPath, nil, nil, false
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if !followFinal && len(parts) == 0 {
				return filepath.Join(curPath, part), curPath, fi, nil, true
			}
			target, err := cur.readlink(part)
			if hops++; hops > maxSymlinkHops || err != nil || target == "" || remote(target, sc.cwd) || strings.HasPrefix(target, `\??\`) || filepath.VolumeName(target) != "" && !filepath.IsAbs(target) || len(curPath)+len(target) > maxPathBytes {
				return "", "", nil, nil, !sc.fail()
			}
			tVol := filepath.VolumeName(target)
			if filepath.IsAbs(target) || os.IsPathSeparator(target[0]) {
				newVol := cmp.Or(tVol, filepath.VolumeName(curPath), filepath.VolumeName(sc.cwd)) + string(filepath.Separator)
				nr, err := openRootDir(newVol)
				if err != nil {
					return "", "", nil, nil, !sc.fail()
				}
				closeAll()
				stack, paths = []dirHandle{nr}, []string{newVol}
			}
			parts = append(strings.FieldsFunc(target[len(tVol):], isSep), parts...)
			continue
		}
		nextPath := filepath.Join(curPath, part)
		if len(parts) == 0 {
			f, ok = sc.openLeaf(cur, part, fi, openMode)
			return nextPath, curPath, fi, f, ok
		} else if !fi.IsDir() {
			return "", "", nil, nil, false
		} else if next, err := cur.openSubDir(part, fi); errors.Is(err, fs.ErrNotExist) {
			return "", curPath, nil, nil, false
		} else if err != nil {
			return "", "", nil, nil, !sc.fail()
		} else {
			stack, paths = append(stack, next), append(paths, nextPath)
		}
	}
	cur, curPath := stack[len(stack)-1], paths[len(paths)-1]
	if fi, err := cur.lstat("."); err == nil {
		f, ok = sc.openLeaf(cur, ".", fi, openMode)
		return curPath, filepath.Dir(curPath), fi, f, ok
	}
	return curPath, filepath.Dir(curPath), nil, nil, false
}
func (sc *scan) openLeaf(cur dirHandle, part string, fi fs.FileInfo, openMode uint8) (*os.File, bool) {
	if openMode == 0 || openMode == 1 && !fi.Mode().IsRegular() || openMode == 2 && !fi.IsDir() {
		return nil, openMode == 0
	}
	f, err := cur.open(part, openMode == 2, fi)
	if err != nil {
		sc.unknown = sc.unknown || !errors.Is(err, fs.ErrNotExist) && (openMode != 2 || !errors.Is(err, fs.ErrPermission))
	}
	return f, err == nil
}
func (sc *scan) walkPath(p string, followFinal bool) (string, string, os.FileInfo, bool) {
	r, par, info, _, ok := sc.walk(p, followFinal, 0)
	return r, par, info, ok
}
func (sc *scan) hasRemoteLink(p string) bool { _, _, _, _ = sc.walkPath(p, true); return sc.unknown }
func (sc *scan) resolveTarget(base, raw string, _ int) (string, bool) {
	if remote(raw, sc.cwd) || filepath.VolumeName(raw) != "" && !filepath.IsAbs(raw) {
		return "", !sc.fail()
	}
	if !filepath.IsAbs(raw) && len(raw) > 0 && os.IsPathSeparator(raw[0]) {
		raw = cmp.Or(filepath.VolumeName(base), filepath.VolumeName(sc.cwd)) + raw
	} else if !filepath.IsAbs(raw) && base != "" {
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
		sc.place(map[bool]string{true: target, false: tParent}[tInfo.IsDir()])
		return res, tInfo
	} else if !sc.unknown && tParent != "" {
		sc.place(tParent)
	}
	return res, nil
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

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func hasDotDot(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/..") || os.IsPathSeparator('\\') && (strings.HasPrefix(p, `..\`) || strings.Contains(p, `\..`))
}
func urlPath(w string) (string, bool, bool) {
	scheme, rest, ok := strings.Cut(w, "://")
	if !ok && len(w) > 5 && strings.EqualFold(w[:5], "file:") && w[5] == '/' {
		scheme, rest, ok = "file", w[5:], true
	}
	if !ok || !isScheme(scheme) || !strings.EqualFold(scheme, "file") {
		return "", ok && isScheme(scheme), false
	}
	if len(rest) > 9 && strings.EqualFold(rest[:9], "localhost") && rest[9] == '/' {
		rest = rest[9:]
	}
	u, err := url.PathUnescape(rest)
	if err != nil || strings.ContainsRune(u, 0) {
		return "", true, true
	} else if len(u) > 2 && u[0] == '/' && u[2] == ':' {
		return u[1:], true, false
	}
	return map[bool]string{true: u, false: "//" + u}[strings.HasPrefix(u, "/")], true, false
}
func isScheme(s string) bool {
	return len(s) > 1 && strings.IndexByte(alnum[:52], s[0]) >= 0 && strings.Trim(s, alnum+"+-.") == ""
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
	if host == "" || name == "" || strings.Trim(host, alnum+"-_.$") != "" {
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
	}
	return map[bool]string{true: "", false: w}[strings.HasPrefix(w, "-")]
}
func isName(s string) bool {
	return s != "" && (s[0] < '0' || s[0] > '9') && strings.Trim(s, alnum+"_") == ""
}
func pathLike(w string) bool {
	return strings.ContainsRune(w, '/') || strings.ContainsRune(w, filepath.Separator) || filepath.IsAbs(w)
}
