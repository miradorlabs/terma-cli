package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mvdan.cc/sh/v3/pattern"
)

const aloneMark = "\x00alone\x00"

// Alone marks paths that reach themselves alone (searchedEnv does not walk below them).
func Alone(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != "" && !isUnknownReach(p) && !strings.HasPrefix(p, aloneMark) {
			p = aloneMark + p
		}
		out = append(out, p)
	}
	return out
}

// Search returns dir and the base folder each glob names before its first wildcard.
func Search(dir string, globs ...string) []string { return searchEnv(dir, os.LookupEnv, globs...) }
func searchEnv(dir string, env func(string) (string, bool), globs ...string) []string {
	out := []string{dir}
	for _, g := range globs {
		alts, ok := expandGlobBraces(globToSlash(g), 32)
		if !ok {
			out = append(out, unknownReach(g))
			continue
		}
		for _, slashed := range alts {
			if strings.HasPrefix(slashed, "~") {
				home, ok := env("HOME")
				if (slashed != "~" && !strings.HasPrefix(slashed, "~/")) || !ok || home == "" {
					out = append(out, unknownReach(g))
					continue
				}
				if slashed == "~" {
					out = append(out, home)
					continue
				}
				slashed = strings.TrimRight(filepath.ToSlash(home), "/") + slashed[1:]
			}
			if p := literalPrefix(slashed); p != "" {
				if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				} else if !strings.HasPrefix(p, "//") {
					p = filepath.FromSlash(p)
				}
				out = append(out, p)
			}
		}
	}
	return out
}
func globToSlash(g string) string {
	if filepath.Separator != '\\' || !strings.ContainsRune(g, '\\') {
		return g
	}
	if !strings.ContainsRune(g, '/') {
		return filepath.ToSlash(g)
	}
	b := []byte(g)
	for i := range b {
		if b[i] == '\\' && (i+1 >= len(b) || strings.IndexByte("*?[]{}", b[i+1]) < 0) {
			b[i] = '/'
		}
	}
	return string(b)
}
func expandGlobBraces(s string, limit int) ([]string, bool) {
	open := strings.IndexByte(s, '{')
	if open < 0 {
		return []string{s}, true
	}
	if strings.Count(s, "{") > limit {
		return nil, false
	}
	closeIdx := strings.IndexByte(s[open+1:], '}')
	if closeIdx < 0 {
		return []string{s}, !strings.ContainsRune(s[open:], '/')
	}
	body := s[open+1 : open+1+closeIdx]
	parts := strings.Split(body, ",")
	if strings.ContainsRune(body, '{') || len(parts) < 2 || len(parts) > limit {
		return nil, false
	}
	var out []string
	for _, p := range parts {
		sub, ok := expandGlobBraces(s[:open]+p+s[open+closeIdx+2:], limit-len(out))
		if !ok || len(out)+len(sub) > limit {
			return nil, false
		}
		out = append(out, sub...)
	}
	return out, true
}
func literalPrefix(glob string) string {
	segs := strings.Split(glob, "/")
	n := slices.IndexFunc(segs, func(s string) bool { return pattern.HasMeta(s, 0) })
	if n < 0 {
		n = len(segs) - 1
	}
	if n == 1 && segs[0] == "" {
		return "/"
	}
	for i := range n {
		segs[i] = unescape(segs[i])
	}
	return strings.Join(segs[:n], "/")
}
func searchedEnv(paths []string, cwd string, env func(string) (string, bool)) []string {
	return searchedBy(paths, cwd, env, time.Now().Add(maxShellTime))
}
func searchedBy(paths []string, cwd string, env func(string) (string, bool), deadline time.Time) []string {
	var sc *scan
	unknownDir, out := "", make([]string, 0, len(paths))
	for _, p := range paths {
		dir, alone := strings.CutPrefix(p, aloneMark)
		if remote(dir, cwd) {
			dir = unknownReach(dir)
		}
		if !slices.Contains(out, dir) {
			out = append(out, dir)
		}
		if alone || dir == "" || isUnknownReach(dir) || unknownDir != "" {
			continue
		}
		if sc == nil {
			sc = newScan(cwd, env, work{deadline: deadline})
		}
		if abs, ok := sc.path(dir, cwd); ok && sc.spend(1) && !sc.hasRemoteLink(abs) {
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				sc.below(abs, maxBelowDepth+2)
			}
		}
		if sc.unknown {
			unknownDir = dir
		}
	}
	if sc != nil {
		for _, p := range sc.out {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	if unknownDir != "" {
		out = append(out, unknownReach(unknownDir))
	}
	return out
}
