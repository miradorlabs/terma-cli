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
	out := slices.Clone(paths)
	for i, p := range out {
		out[i] = pick(p != "" && !isUnknownReach(p) && !strings.HasPrefix(p, aloneMark), aloneMark+p, p)
	}
	return out
}

// Search returns dir and the base folder each glob names before its first wildcard.
func Search(dir string, globs ...string) []string { return searchEnv(dir, os.LookupEnv, globs...) }
func searchEnv(dir string, env func(string) (string, bool), globs ...string) []string {
	out := []string{dir}
	for _, g := range globs {
		if alts, ok := expandGlobBraces(globToSlash(g), 32); !ok {
			out = append(out, unknownReach(g))
		} else {
			for _, slashed := range alts {
				if strings.HasPrefix(slashed, "~") {
					home, ok := env("HOME")
					if (slashed != "~" && !strings.HasPrefix(slashed, "~/")) || !ok || home == "" || slashed == "~" {
						out = append(out, pick(slashed == "~" && ok && home != "", home, unknownReach(g)))
						continue
					}
					slashed = strings.TrimRight(filepath.ToSlash(home), "/") + slashed[1:]
				}
				if p := literalPrefix(slashed); p != "" {
					out = append(out, pick(!filepath.IsAbs(p), filepath.Join(dir, p), pick(!strings.HasPrefix(p, "//"), filepath.FromSlash(p), p)))
				}
			}
		}
	}
	return out
}
func globToSlash(g string) string {
	if filepath.Separator != '\\' || !strings.ContainsRune(g, '\\') {
		return g
	}
	b, hasSlash := []byte(g), strings.ContainsRune(g, '/')
	for i := range b {
		if b[i] == '\\' && (!hasSlash || i+1 >= len(b) || strings.IndexByte("*?[]{}", b[i+1]) < 0) {
			b[i] = '/'
		}
	}
	return string(b)
}
func expandGlobBraces(s string, limit int) ([]string, bool) {
	if open := strings.IndexByte(s, '{'); open < 0 || strings.Count(s, "{") > limit {
		return pick(open < 0, []string{s}, nil), open < 0
	} else if closeIdx := strings.IndexByte(s[open+1:], '}'); closeIdx < 0 {
		return []string{s}, !strings.ContainsRune(s[open:], '/')
	} else if body, parts, out := s[open+1:open+1+closeIdx], strings.Split(s[open+1:open+1+closeIdx], ","), []string(nil); strings.ContainsRune(body, '{') || len(parts) < 2 || len(parts) > limit {
		return nil, false
	} else {
		for _, p := range parts {
			sub, ok := expandGlobBraces(s[:open]+p+s[open+closeIdx+2:], limit-len(out))
			if !ok || len(out)+len(sub) > limit {
				return nil, false
			}
			out = append(out, sub...)
		}
		return out, true
	}
}
func literalPrefix(glob string) string {
	segs := strings.Split(glob, "/")
	n := slices.IndexFunc(segs, func(s string) bool { return pattern.HasMeta(s, 0) })
	if n == 1 && segs[0] == "" {
		return "/"
	}
	n = pick(n < 0, len(segs)-1, n)
	for i := range n {
		segs[i] = unescape(segs[i])
	}
	return strings.Join(segs[:n], "/")
}
func appendUnique(out []string, items ...string) []string {
	for _, p := range items {
		out = pick(slices.Contains(out, p), out, append(out, p))
	}
	return out
}
func searchedBy(paths []string, cwd string, env func(string) (string, bool), deadline time.Time) []string {
	sc, unknownDir, out := newScan(cwd, env, work{deadline: deadline}), "", make([]string, 0, len(paths))
	for _, p := range paths {
		dir, alone := strings.CutPrefix(p, aloneMark)
		dir = pick(remote(dir, cwd), unknownReach(dir), dir)
		if out = appendUnique(out, dir); alone || dir == "" || isUnknownReach(dir) || unknownDir != "" {
			continue
		}
		if abs, ok := sc.path(dir, cwd); ok && sc.spend(1) {
			if r, _, info, found := sc.walkPath(abs, true); found && info.IsDir() {
				sc.below(r, maxBelowDepth+2)
			}
		}
		unknownDir = pick(sc.unknown, dir, unknownDir)
	}
	out = appendUnique(out, sc.out...)
	return pick(unknownDir != "", append(out, unknownReach(unknownDir)), out)
}
