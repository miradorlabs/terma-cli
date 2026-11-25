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
	"unicode/utf16"
)

const maxSymlinkHops, maxPathComponents, maxPathBytes, alnum = 8, 128, 4096, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func (sc *scan) candidate(t string, r role) {
	if v := pick(r.whole(), t, shellValue(t)); v != t {
		sc.candidatePath(t, r, true)
		t = v
	}
	if p, isURL, bad := urlPath(t); bad && sc.fail() {
		return
	} else if isURL {
		if filepath.Separator == '/' {
			sc.candidatePath(t, r, true)
		}
		t = p
	}
	sc.candidatePath(t, r, false)
}
func (sc *scan) candidatePath(t string, r role, fallback bool) {
	t = pick(filepath.Separator == '/' && strings.HasPrefix(t, "///") && strings.TrimLeft(t, "/") != "", "/"+strings.TrimLeft(t, "/"), t)
	slashed, rem := pathLike(t), remote(t, sc.cwd)
	if t == "" || !slashed && r.needsSlash() {
		return
	} else if rem {
		sc.unknown = sc.unknown || (share(t) != "" || len(t) > 2 && !strings.ContainsAny(t, " \t\n") && strings.TrimLeft(t, `/\`) != "") && r != program && r != data && r != part
		return
	} else if len(t) > maxPathBytes || !filepath.IsAbs(t) && len(t)+max(len(sc.dir), len(sc.cwd))+1 > maxPathBytes || !slashed && (filepath.Separator != '\\' && len(t) > 255 || filepath.Separator == '\\' && len(utf16.Encode([]rune(t))) > 255) {
		sc.unknown = sc.unknown || r == target || r == physTarget || r == folder || slashed && r != program && r != data && r != part
		return
	}
	c := candidate{t, sc.dir, r}
	if t == "~" || strings.HasPrefix(t, "~/") {
		home, _ := sc.homeDir()
		c.dir = "\x00" + home
	}
	res, seen := sc.examined[c]
	if !seen && !sc.spend(1) {
		return
	} else if !seen {
		for i, d := range pick(sc.dir == sc.cwd, []string{sc.dir}, []string{sc.dir, sc.cwd}) {
			if p, info := sc.reachRole(t, d, r); p != "" {
				if res.named = res.named || info != nil; info != nil && info.IsDir() {
					res.dir, res.folders = pick(i == 0, p, res.dir), append(res.folders, p)
				}
			} else if (r == creates || slashed && (r == plain || r == folder || r == slashedOnly)) && !res.named {
				if p, ok := sc.path(t, d); ok {
					if rPath, parent, _, found := sc.walkPath(p, true); !found && !sc.unknown && parent != "" && (!fallback || filepath.Dir(rPath) == parent && strings.ContainsAny(strings.TrimRight(t, `/\`), `/\`)) {
						// In historical mode, place the missing path itself rather than its
						// nearest existing ancestor: the ancestor (often "/" or a home folder)
						// may be nobody's while the path once belonged to an organization.
						// Missing absolute paths are therefore unknown by design, including
						// strings that only look like paths (such as an API route).
						if sc.historical && rPath != "" {
							sc.place(rPath)
						} else {
							sc.place(parent)
						}
					}
				}
			}
		}
		sc.examined[c] = res
	}
	if sc.reads != nil && res.named && (r == plain || r == folder || r == part) {
		sc.reads.named, sc.reads.folders = true, append(sc.reads.folders, res.folders...)
	}
	if (r == target || r == physTarget) && res.dir != "" {
		sc.prev, sc.dir = sc.dir, res.dir
	}
}

// walk traverses p component-by-component via dirHandle (openat O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK on Unix,
// os.Root on Windows), preventing TOCTOU symlink swaps and FIFO hangs. openMode: 0=stat, 1=openFile, 2=openDir, 3=sysBin.
func (sc *scan) walk(p string, followFinal bool, openMode uint8) (resolved, parent string, info os.FileInfo, f *os.File, ok bool) {
	if !filepath.IsAbs(p) && !filepath.IsAbs(sc.dir) {
		return "", "", nil, nil, false
	} else if p = pick(filepath.IsAbs(p), p, sc.dir+string(filepath.Separator)+p); sc.unknown || remote(p, sc.cwd) || len(p) > maxPathBytes {
		return "", "", nil, nil, !sc.fail()
	}
	vol := filepath.VolumeName(p)
	root, err := openRootDir(vol + string(filepath.Separator))
	if err != nil {
		sc.unknown = sc.unknown || !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid)
		return "", "", nil, nil, false
	}
	stack, paths := []dirHandle{root}, []string{vol + string(filepath.Separator)}
	closeStack := func() {
		for _, h := range stack {
			h.close()
		}
	}
	defer closeStack()
	for parts, hops := strings.FieldsFunc(p[len(vol):], isSep), 0; len(parts) > 0; {
		if len(parts)+len(stack) > maxPathComponents || !sc.spendStat(1) {
			return "", "", nil, nil, !sc.fail()
		}
		part, cur, curPath := parts[0], stack[len(stack)-1], paths[len(paths)-1]
		if parts = parts[1:]; part == "" || part == "." || part == ".." {
			if part == ".." && len(stack) > 1 {
				cur.close()
				stack, paths = stack[:len(stack)-1], paths[:len(paths)-1]
			}
			continue
		}
		fi, err := cur.lstat(part)
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
			return "", "", nil, nil, !sc.fail()
		} else if err != nil {
			dot, missing := slices.Contains(parts, "..") || slices.Contains(parts, "."), filepath.Join(curPath, part)
			sc.unknown = sc.unknown || dot && sc.created[pick(filepath.Separator == '\\', strings.ToLower(missing), missing)]
			return pick(dot, "", filepath.Join(append([]string{curPath, part}, parts...)...)), pick(dot, "", curPath), nil, nil, false
		} else if openMode == 3 && !isSysBin(filepath.Join(curPath, part), fi) {
			return "", "", nil, nil, false
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
				closeStack()
				stack, paths = []dirHandle{nr}, []string{newVol}
			}
			parts = append(strings.FieldsFunc(target[len(tVol):], isSep), parts...)
			continue
		}
		if nextPath := filepath.Join(curPath, part); len(parts) == 0 {
			f, ok = sc.openLeaf(cur, part, fi, openMode)
			return nextPath, curPath, fi, f, ok
		} else if !fi.IsDir() {
			return "", "", nil, nil, false
		} else if next, err := cur.openSubDir(part, fi); err == nil {
			stack, paths = append(stack, next), append(paths, nextPath)
		} else {
			sc.unknown = sc.unknown || !errors.Is(err, fs.ErrNotExist)
			return "", pick(errors.Is(err, fs.ErrNotExist), curPath, ""), nil, nil, false
		}
	}
	if fi, err := stack[len(stack)-1].lstat("."); err == nil {
		f, ok = sc.openLeaf(stack[len(stack)-1], ".", fi, openMode)
		return paths[len(paths)-1], filepath.Dir(paths[len(paths)-1]), fi, f, ok
	}
	return "", "", nil, nil, !sc.fail()
}
func (sc *scan) openLeaf(cur dirHandle, part string, fi fs.FileInfo, openMode uint8) (*os.File, bool) {
	if openMode == 0 || openMode == 3 || openMode == 1 && !fi.Mode().IsRegular() || openMode == 2 && !fi.IsDir() {
		return nil, openMode == 0 || openMode == 3
	}
	f, err := cur.open(part, openMode == 2, fi)
	sc.unknown = sc.unknown || err != nil && !errors.Is(err, fs.ErrNotExist) && (openMode != 2 || !errors.Is(err, fs.ErrPermission))
	return f, err == nil
}
func (sc *scan) walkPath(p string, followFinal bool) (string, string, os.FileInfo, bool) {
	r, par, info, _, ok := sc.walk(p, followFinal, 0)
	return r, par, info, ok
}
func (sc *scan) hasRemoteLink(p string) bool { _, _, _, _ = sc.walkPath(p, true); return sc.unknown }
func (sc *scan) resolveJoined(base, raw string, logical bool) (string, bool) {
	if vol := filepath.VolumeName(raw); remote(raw, sc.cwd) || vol != "" && !filepath.IsAbs(raw) {
		sc.unknown = sc.unknown || remote(raw, sc.cwd) || len(vol) != 2 || vol[1] != ':' || strings.IndexByte(alnum[:52], vol[0]) >= 0
		return "", false
	}
	if !filepath.IsAbs(raw) && len(raw) > 0 && os.IsPathSeparator(raw[0]) {
		raw = cmp.Or(filepath.VolumeName(base), filepath.VolumeName(sc.cwd)) + raw
	} else if !filepath.IsAbs(raw) && base != "" {
		raw = strings.TrimRight(base, string(filepath.Separator)) + string(filepath.Separator) + raw
	}
	if len(raw) > maxPathBytes && sc.fail() || logical && !filepath.IsAbs(raw) {
		return "", false
	} else if !logical && !slices.ContainsFunc(strings.FieldsFunc(raw, isSep), func(s string) bool { return s == "." || s == ".." }) {
		return filepath.Clean(raw), true
	} else if !logical {
		res, _, _, found := sc.walkPath(raw, false)
		return res, found || !sc.unknown && res != ""
	}
	vol, sep, out := cmp.Or(filepath.VolumeName(raw), share(raw)), string(filepath.Separator), []string(nil)
	parts := strings.FieldsFunc(raw[len(vol):], isSep)
	if len(parts) > maxPathComponents {
		return "", !sc.fail()
	}
	for _, part := range parts {
		if part != "" && part != "." && part != ".." {
			out = append(out, part)
		} else if cur := vol + sep + strings.Join(out, sep); part == ".." && cur != "" {
			if t, _, info, found := sc.walkPath(cur, true); !found || !info.IsDir() {
				sc.unknown = sc.unknown || sc.historical || sc.created[pick(filepath.Separator == '\\', strings.ToLower(t), t)]
				return "", false
			} else if _, _, li, ok := sc.walkPath(cur, false); ok && li.Mode()&fs.ModeSymlink != 0 {
				sc.place(t)
			}
			out = out[:max(len(out)-1, 0)]
		}
	}
	return vol + sep + strings.Join(out, sep), true
}
func (sc *scan) reach(w, dir string) (string, os.FileInfo) { return sc.reachRole(w, dir, plain) }
func (sc *scan) reachRole(w, dir string, r role) (res string, _ os.FileInfo) {
	if p, ok := sc.resolvePath(w, dir, r == target); !ok {
		return "", nil
	} else if r0, parent, info, found := sc.walkPath(p, false); !found {
		sc.unknown = sc.unknown || (r == target || r == physTarget) && sc.created[pick(filepath.Separator == '\\', strings.ToLower(r0), r0)]
		if sc.historical && (r == target || r == physTarget) {
			if r0 != "" {
				sc.place(r0)
			} else {
				sc.fail()
			}
		}
		return "", nil
	} else if res = r0; info.IsDir() {
		sc.place(res)
		return pick(r == target, p, res), info
	} else if sc.place(parent); info.Mode()&fs.ModeSymlink == 0 {
		return res, info
	} else if dst, tParent, tInfo, tFound := sc.walkPath(p, true); tFound {
		sc.place(pick(tInfo.IsDir(), dst, tParent))
		return pick(r == physTarget, dst, pick(r == target, p, res)), tInfo
	} else if !sc.unknown && tParent != "" {
		sc.place(pick(sc.historical && dst != "", dst, tParent))
	}
	return res, nil
}
func (sc *scan) path(w, dir string) (string, bool) { return sc.resolvePath(w, dir, false) }
func (sc *scan) resolvePath(w, dir string, logical bool) (string, bool) {
	if w == "~" || strings.HasPrefix(w, "~/") {
		home, ok := sc.homeDir()
		if !ok || home == "" || remote(home, sc.cwd) {
			sc.unknown = sc.unknown || remote(home, sc.cwd) || sc.isUnresolvedVar("HOME")
			return "", false
		}
		return sc.resolveJoined(home, strings.TrimLeft(w[1:], "/"), logical)
	} else if remote(w, sc.cwd) || !filepath.IsAbs(w) && dir == "" {
		return "", false
	}
	return sc.resolveJoined(dir, w, logical)
}
func urlPath(w string) (string, bool, bool) {
	scheme, rest, ok := strings.Cut(w, "://")
	if !ok && len(w) > 5 && strings.EqualFold(w[:5], "file:") && w[5] == '/' {
		scheme, rest, ok = "file", w[5:], true
	}
	if ok = ok && len(scheme) > 1 && strings.IndexByte(alnum[:52], scheme[0]) >= 0 && strings.Trim(scheme, alnum+"+-.") == ""; !ok || !strings.EqualFold(scheme, "file") {
		return "", ok, false
	}
	rest, _, _ = strings.Cut(strings.Split(rest, "?")[0], "#")
	rest = pick(len(rest) > 9 && strings.EqualFold(rest[:9], "localhost") && rest[9] == '/', rest[min(len(rest), 9):], rest)
	u, err := url.PathUnescape(rest)
	bad := err != nil || strings.ContainsRune(u, 0)
	return pick(bad, "", pick(filepath.Separator == '\\' && len(u) > 2 && u[0] == '/' && u[2] == ':', u[min(len(u), 1):], pick(strings.HasPrefix(u, "/"), u, "//"+u))), true, bad
}

// remote reports whether p starts with two separators (UNC/device path) outside cwd's share.
func remote(p, cwd string) bool {
	return len(p) >= 2 && os.IsPathSeparator(p[0]) && os.IsPathSeparator(p[1]) && (share(p) == "" || !strings.EqualFold(share(p), share(cwd)))
}
func share(p string) string {
	if i := pick(len(p) >= 2 && os.IsPathSeparator(p[0]) && os.IsPathSeparator(p[1]), strings.IndexFunc(p[min(len(p), 2):], isSep), -1); i > 0 {
		host, rest := p[2:2+i], p[3+i:]
		if j := strings.IndexFunc(rest, isSep); j != 0 && rest != "" && strings.Trim(host, alnum+"-_.$") == "" {
			return "//" + host + "/" + pick(j > 0, rest[:max(j, 0)], rest)
		}
	}
	return ""
}
func isSep(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) }
func shellValue(w string) string {
	if name, v, ok := strings.Cut(w, "="); ok && (strings.HasPrefix(w, "-") || isName(name)) {
		return v
	}
	return pick(len(w) > 2 && w[0] == '-' && w[1] != '-', w[min(len(w), 2):], pick(strings.HasPrefix(w, "-"), "", w))
}
func isName(s string) bool {
	return s != "" && (s[0] < '0' || s[0] > '9') && strings.Trim(s, alnum+"_") == ""
}
func pathLike(w string) bool {
	return strings.ContainsRune(w, '/') || strings.ContainsRune(w, filepath.Separator) || filepath.IsAbs(w)
}
