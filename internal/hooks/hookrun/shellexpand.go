package hookrun

import (
	"io/fs"
	"iter"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

var escaped = regexp.MustCompile(`(?s)\\(.)`)

func (sc *scan) expandWord(w *syntax.Word, fn func(*expand.Config, *syntax.Word) (string, error)) (string, error) {
	if !sc.charge(sc.size(w)) {
		return "", errOpaque
	}
	defer func(prev bool) { sc.opaque = prev }(sc.opaque)
	sc.opaque = false
	out, err := fn(sc.cfg, w)
	return pick(err != nil || sc.opaque, "", out), pick(err != nil || sc.opaque, errOpaque, error(nil))
}
func (sc *scan) addWord(f string, r role) {
	sc.candidate(f, r)
	if (r == plain || r == slashedOnly) && strings.ContainsFunc(f, isFragmentSeparator) {
		sc.fragments(f, pick(strings.ContainsFunc(f, unicode.IsSpace), data, pick(r == plain, part, r)))
	}
}
func (sc *scan) arg(w *syntax.Word, r role) {
	if sc.words++; (sc.unknown || sc.words > maxShellWords) && sc.fail() {
		return
	} else if sc.substitutions(w); w.Lit() != "" && !strings.ContainsAny(w.Lit(), `\~{*?[`) {
		sc.addWord(w.Lit(), r)
		return
	}
	braced := *w
	split := syntax.SplitBraces(&braced)
	for b, err := range expand.BracesSeq(sc.cfg, &braced) {
		if (err != nil || split && (len(braced.Parts) > 2*maxShellBraces || !sc.spend(1))) && sc.fail() {
			return
		}
		sc.expand(b, r)
	}
}
func (sc *scan) expand(w *syntax.Word, r role) {
	if r != program && mayGlob(w) {
		if pat, err := sc.expandWord(w, expand.Pattern); err == nil && pattern.HasMeta(pat, 0) && !remote(pat, sc.cwd) && sc.glob(pat, r) {
			return
		}
	}
	fields, err := sc.fields(w)
	sc.unknown = sc.unknown || err != nil && (r == target || r == physTarget || r == folder || sc.mustResolveWord(w) && r != program && r != data && r != part)
	for _, f := range fields {
		sc.addWord(f, r)
	}
}
func (sc *scan) glob(pat string, r role) bool {
	if home, ok := sc.homeDir(); (pat == "~" || strings.HasPrefix(pat, "~/")) && ok && home != "" {
		pat = strings.TrimRight(filepath.ToSlash(home), "/") + pat[1:]
	}
	segs := strings.Split(pat, "/")
	if len(slices.DeleteFunc(slices.Clone(segs), func(s string) bool { return !pattern.HasMeta(s, 0) })) > maxGlobSegments && sc.fail() {
		return true
	}
	dirs := pick(sc.dir == sc.cwd, []string{sc.dir}, []string{sc.dir, sc.cwd})
	if root := segs[0]; root == "" || filepath.VolumeName(root) != "" {
		dirs, segs = []string{root + string(filepath.Separator)}, segs[1:]
	}
	if slices.Contains(segs, "**") {
		return sc.globstar(dirs, segs, r)
	}
	found := false
	for _, dir := range dirs {
		c := candidate{pat, dir, globbed}
		res, seen := sc.examined[c]
		if !seen {
			at := []string{dir}
			for i, seg := range segs {
				if !pattern.HasMeta(seg, 0) {
					for j := range at {
						at[j] = filepath.Join(at[j], unescape(seg))
					}
					continue
				}
				expr, err := pattern.Regexp(seg, pattern.Filenames|pattern.EntireString)
				rx, err2 := regexp.Compile(expr)
				if err != nil || err2 != nil {
					return false
				}
				var next []string
				for _, d := range at {
					if !sc.hasRemoteLink(d) {
						for _, e := range sc.readDir(d) {
							if rx.MatchString(e.Name()) && (i == len(segs)-1 || e.IsDir() || e.Type()&fs.ModeSymlink != 0) {
								next = append(next, filepath.Join(d, e.Name()))
							}
						}
					}
				}
				at = next
			}
			for _, m := range at {
				if !sc.spend(1) {
					break
				} else if p, info := sc.reach(m, ""); p != "" {
					res.named, res.folders = true, pick(info != nil && info.IsDir(), append(res.folders, m), res.folders)
				}
			}
			sc.examined[c] = res
		}
		sc.recordCached(res, r)
		found = found || res.named
	}
	return found || sc.unknown
}
func (sc *scan) recordCached(res cached, r role) {
	if sc.reads != nil && res.named && (r == plain || r == folder || r == part) {
		sc.reads.named, sc.reads.folders = true, append(sc.reads.folders, res.folders...)
	}
}
func (sc *scan) globstar(dirs, segs []string, r role) bool {
	found, prefix := false, literalPrefix(strings.Join(segs, "/"))
	for _, dir := range dirs {
		if p := filepath.Join(dir, prefix); sc.spend(1) {
			if resolved, _, info, ok := sc.walkPath(p, true); ok && info.IsDir() {
				found = true
				sc.place(resolved)
				sc.recordCached(cached{named: true, folders: []string{resolved}}, r)
				sc.below(resolved, maxBelowDepth+2)
			}
		}
	}
	return found || sc.unknown
}
func (sc *scan) mustResolveWord(w *syntax.Word) bool {
	found := strings.HasPrefix(leadingLit(w), "~")
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Lit:
			found = found || pathLike(n.Value)
		case *syntax.SglQuoted:
			found = found || pathLike(n.Value)
		case *syntax.ParamExp:
			found = found || n.Param != nil && !sc.loopVars[n.Param.Value] && sc.isUnresolvedVar(n.Param.Value)
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp:
			return false
		}
		return !found
	})
	return found
}
func unescape(seg string) string { return escaped.ReplaceAllString(seg, "$1") }
func mayGlob(w *syntax.Word) bool {
	return slices.ContainsFunc(w.Parts, func(p syntax.WordPart) bool {
		lit, ok := p.(*syntax.Lit)
		_, param := p.(*syntax.ParamExp)
		_, ext := p.(*syntax.ExtGlob)
		return ok && strings.ContainsAny(lit.Value, "*?[") || param || ext
	})
}
func isFragmentSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("'\"`()[]{}<>,;:@=", r)
}
func fragments(text string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := -1
		for i, r := range text + " " {
			drive := r == ':' && start >= 0 && i == start+1 && i+1 < len(text) && (text[i+1] == '/' || text[i+1] == '\\') && unicode.IsLetter(rune(text[start]))
			url := r == ':' && start >= 0 && i-start > 1 && strings.HasPrefix(text[i+1:], "//") && strings.IndexByte(alnum[:52], text[start]) >= 0 && strings.Trim(text[start:i], alnum+"+-.") == ""
			if isFragmentSeparator(r) && !drive && !url {
				if start >= 0 && !yield(text[start:i]) {
					return
				}
				start = -1
			} else if start < 0 {
				start = i
			}
		}
	}
}
