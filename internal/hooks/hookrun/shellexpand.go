package hookrun

import (
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

func (sc *scan) document(w *syntax.Word) (string, error) {
	if !sc.charge(sc.size(w)) {
		return "", errOpaque
	}
	return expand.Document(sc.cfg, w)
}
func (sc *scan) pattern(w *syntax.Word) (string, error) {
	if !sc.charge(sc.size(w)) {
		return "", errOpaque
	}
	return expand.Pattern(sc.cfg, w)
}
func (sc *scan) arg(w *syntax.Word, r role) {
	if sc.words++; sc.unknown || sc.words > maxShellWords {
		sc.unknown = true
		return
	}
	sc.substitutions(w)
	if l := w.Lit(); l != "" && !strings.ContainsAny(l, `\~{*?[`) {
		sc.candidate(l, r)
		sc.pieces(l, r)
		return
	}
	for w, err := range sc.braces(w) {
		if err != nil {
			return
		}
		sc.expand(w, r)
	}
}
func (sc *scan) expand(w *syntax.Word, r role) {
	if r != program && mayGlob(w) {
		if pat, err := sc.pattern(w); err == nil && pattern.HasMeta(pat, 0) && sc.glob(pat, r) {
			return
		}
	}
	fields, err := sc.fields(w)
	if err != nil && r.mustResolve(slashed(w)) {
		sc.unknown = true
	}
	for _, f := range fields {
		sc.candidate(f, r)
		sc.pieces(f, r)
	}
}
func (sc *scan) pieces(f string, r role) {
	switch {
	case r != plain && r != slashedOnly || !strings.ContainsFunc(f, isFragmentSeparator):
	case strings.ContainsFunc(f, unicode.IsSpace):
		sc.fragments(f, data)
	case r == plain:
		sc.fragments(f, part)
	default:
		sc.fragments(f, slashedOnly)
	}
}
func (sc *scan) glob(pat string, r role) bool {
	if pat == "~" || strings.HasPrefix(pat, "~/") {
		if home, ok := sc.lookup("HOME"); ok && home != "" {
			pat = strings.TrimRight(filepath.ToSlash(home), "/") + pat[1:]
		}
	}
	if sc.away(pat, r) {
		return true
	}
	segs := strings.Split(pat, "/")
	if len(slices.DeleteFunc(slices.Clone(segs), func(s string) bool { return !pattern.HasMeta(s, 0) })) > maxGlobSegments {
		sc.unknown = true
		return true
	}
	dirs := []string{sc.dir, sc.cwd}
	if sc.dir == sc.cwd {
		dirs = dirs[:1]
	}
	if root := segs[0]; root == "" || filepath.VolumeName(root) != "" {
		dirs, segs = []string{root + string(filepath.Separator)}, segs[1:]
	}
	if slices.Contains(segs, "**") {
		return sc.globstar(dirs, segs, r)
	}
	found := false
	for _, dir := range dirs {
		c := candidate{pat, dir, globbed}
		if res, seen := sc.examined[c]; seen {
			sc.recordCached(res, r)
			found = found || res.named
			continue
		}
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
				if sc.hasRemoteLink(d) {
					continue
				}
				for _, e := range sc.readDir(d) {
					if rx.MatchString(e.Name()) && (i == len(segs)-1 || e.IsDir() || e.Type()&fs.ModeSymlink != 0) {
						next = append(next, filepath.Join(d, e.Name()))
					}
				}
			}
			at = next
		}
		var res cached
		for _, m := range at {
			if !sc.spend(1) {
				break
			}
			if p, info := sc.reach(m, ""); p != "" {
				res.named = true
				if info != nil && info.IsDir() {
					res.folders = append(res.folders, m)
				}
			}
		}
		sc.examined[c] = res
		sc.recordCached(res, r)
		found = found || res.named
	}
	return found || sc.unknown
}
func (sc *scan) recordCached(res cached, r role) {
	if sc.reads != nil && res.named && (r == plain || r == folder || r == part) {
		sc.reads.named = true
		sc.reads.folders = append(sc.reads.folders, res.folders...)
	}
}
func (sc *scan) globstar(dirs, segs []string, r role) bool {
	found, prefix := false, literalPrefix(strings.Join(segs, "/"))
	for _, dir := range dirs {
		p := filepath.Join(dir, prefix)
		if !sc.spend(1) {
			break
		}
		if sc.hasRemoteLink(p) {
			continue
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			found = true
			sc.place(p)
			sc.recordCached(cached{named: true, folders: []string{p}}, r)
			sc.below(p, maxBelowDepth+2)
		}
	}
	return found || sc.unknown
}

func unescape(seg string) string {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		if seg[i] == '\\' && i+1 < len(seg) {
			i++
		}
		b.WriteByte(seg[i])
	}
	return b.String()
}

func mayGlob(w *syntax.Word) bool {
	return slices.ContainsFunc(w.Parts, func(p syntax.WordPart) bool {
		switch p := p.(type) {
		case *syntax.Lit:
			return strings.ContainsAny(p.Value, "*?[")
		case *syntax.ParamExp, *syntax.ExtGlob:
			return true
		}
		return false
	})
}

func slashed(w *syntax.Word) bool {
	if len(w.Parts) == 0 {
		return false
	}
	lead, ok := w.Parts[0].(*syntax.Lit)
	found := ok && strings.HasPrefix(lead.Value, "~")
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Lit:
			found = found || pathLike(n.Value)
		case *syntax.SglQuoted:
			found = found || pathLike(n.Value)
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp:
			return false
		}
		return !found
	})
	return found
}

const fragmentSeparators = "'\"`()[]{}<>,;:@="

func isFragmentSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(fragmentSeparators, r)
}

func fragments(text string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := -1
		for i, r := range text + " " {
			drive := r == ':' && start >= 0 && i == start+1 && i+1 < len(text) && (text[i+1] == '/' || text[i+1] == '\\') && unicode.IsLetter(rune(text[start]))
			url := r == ':' && start >= 0 && strings.HasPrefix(text[i+1:], "//") && isScheme(text[start:i])
			switch {
			case isFragmentSeparator(r) && !drive && !url:
				if start >= 0 && !yield(text[start:i]) {
					return
				}
				start = -1
			case start < 0:
				start = i
			}
		}
	}
}
