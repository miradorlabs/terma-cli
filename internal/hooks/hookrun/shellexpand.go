package hookrun

import (
	"errors"
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

// How ShellPaths expands a word (mvdan.cc/sh/v3/expand) where the command is, and what of
// it may be a path.

// maxGlobSegments bounds the segments of a glob holding a wildcard: past it
// (go/pkg/mod/*/*/*/*/*.go) a walk could read a whole tree, and the reach is not known.
const maxGlobSegments = 3

// errOpaque is an expansion ShellPaths cannot resolve: a command substitution's output.
var errOpaque = errors.New("an expansion ShellPaths cannot resolve")

// Get is the variable name where the command is now, for expand: unset when not known,
// which expand (NoUnset) takes for an error unless the word gives a fallback (${X:-y}).
func (sc *scan) Get(name string) expand.Variable {
	if v, ok := sc.lookup(name); ok {
		return expand.Variable{Set: true, Kind: expand.String, Str: v}
	}
	return expand.Variable{}
}

// Each lists no variables: expand asks for each it expands.
func (sc *scan) Each(func(string, expand.Variable) bool) {}

// lookup is the value of the variable name where the command is now: PWD and OLDPWD as its
// cds left them, an assignment earlier in the command, else the hook's environment, HOME
// falling back to the home directory.
func (sc *scan) lookup(name string) (string, bool) {
	switch {
	case name == "PWD":
		return sc.dir, sc.dir != ""
	case name == "OLDPWD" && sc.prev != "":
		return sc.prev, true
	}
	if v, ok := sc.vars[name]; ok {
		if v == nil {
			return "", false
		}
		return *v, true
	}
	if v, ok := sc.env(name); ok {
		return v, true
	}
	if name == "HOME" {
		home, err := os.UserHomeDir()
		return home, err == nil
	}
	return "", false
}

// substitutionSize bounds what a command substitution ShellPaths knows writes (a path), or
// an arithmetic expansion.
const substitutionSize = 4096

// size bounds the bytes w may expand to, before it is: its literal text, each variable's
// value, a pattern replacement's as if each of its bytes were replaced, a fallback's word,
// and each substitution's output. A size past maxShellBytes is maxShellBytes+1.
func (sc *scan) size(w *syntax.Word) int {
	n := 0
	syntax.Walk(w, func(node syntax.Node) bool {
		switch x := node.(type) {
		case *syntax.Lit:
			n += len(x.Value)
		case *syntax.SglQuoted:
			n += len(x.Value)
		case *syntax.ParamExp:
			n += sc.paramSize(x)
			return false
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp:
			n += substitutionSize
			return false
		}
		return n <= maxShellBytes
	})
	return min(n, maxShellBytes+1)
}

// paramSize bounds the bytes the parameter expansion pe may expand to (size).
func (sc *scan) paramSize(pe *syntax.ParamExp) int {
	var v string
	if pe.Param != nil {
		v, _ = sc.lookup(pe.Param.Value)
		if pe.Excl {
			v, _ = sc.lookup(v) // ${!name}: the variable name holds the name of
		}
	}
	n := len(v)
	if pe.Repl != nil && pe.Repl.With != nil {
		if with := sc.size(pe.Repl.With); n > 0 && with >= (maxShellBytes+1)/n {
			n = maxShellBytes + 1
		} else {
			n *= 1 + with
		}
	}
	if pe.Exp != nil && pe.Exp.Word != nil {
		n += sc.size(pe.Exp.Word)
	}
	return min(n, maxShellBytes+1)
}

// fields, document and pattern expand w as expand's functions of those names do, each once
// the byte budget allows for what w may expand to (size), fields for each word its braces
// make; past it, w is not resolved and the reach is not known.
func (sc *scan) fields(w *syntax.Word) ([]string, error) {
	var out []string
	for b, err := range sc.braces(w) {
		if err != nil || !sc.charge(sc.size(b)) {
			return nil, errOpaque
		}
		f, err := expand.Fields(sc.cfg, b)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	return out, nil
}

// braces are the words w's braces make, each a lookup, or w itself when it holds none. Past
// maxShellBraces or the scan's bounds they end in an error, and the reach is not known.
func (sc *scan) braces(w *syntax.Word) iter.Seq2[*syntax.Word, error] {
	return func(yield func(*syntax.Word, error) bool) {
		n := 0
		for _, p := range w.Parts {
			if lit, ok := p.(*syntax.Lit); ok {
				n += strings.Count(lit.Value, "{")
			}
		}
		braced := *w // SplitBraces replaces its parts
		switch {
		case n > maxShellBraces:
			sc.unknown = true
			yield(nil, errOpaque)
		case n == 0 || !syntax.SplitBraces(&braced):
			yield(w, nil)
		default:
			for b, err := range expand.BracesSeq(sc.cfg, &braced) {
				if err != nil || !sc.spend(1) {
					sc.unknown = true
					yield(nil, errOpaque)
					return
				}
				if !yield(b, nil) {
					return
				}
			}
		}
	}
}

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

// arg reads w, a word of a command in role r: its substitutions, which run first, then each
// word its braces make.
func (sc *scan) arg(w *syntax.Word, r role) {
	if sc.words++; sc.unknown || sc.words > maxShellWords {
		sc.unknown = true
		return
	}
	sc.substitutions(w)
	if l := w.Lit(); l != "" && !strings.ContainsAny(l, `\~{*?[`) {
		sc.candidate(l, r) // nothing to expand
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

// expand reads w, a word with no braces left, in role r: a glob is what it matches, if
// anything, else each field w expands to is a candidate, and its pieces too. A word that
// cannot be resolved is unknown when it may be a path.
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

// pieces reads the fragments of f, an argument (r plain) or an assignment's value (r
// slashedOnly) as expanded, when it has any: prose's, holding whitespace (git commit -m "fix
// a/b"), as data; else an argument's as parts (-v ../x:/src, -d @../x) and a value's each as a
// value of its own (PYTHONPATH=.:../x).
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

// glob reaches what pat, a word in role r, matches from where the command is and from the
// cwd, walked a segment at a time so that only the folders a wildcard is in are read
// (expand's own glob reads each folder on the way, to see that it is there), each entry read
// and each match a lookup: a match's folder, or a matched folder itself, each an operand.
// False when nothing matches, and the word is taken as written.
func (sc *scan) glob(pat string, r role) bool {
	segs := strings.Split(pat, "/")
	if len(slices.DeleteFunc(slices.Clone(segs), func(s string) bool { return !pattern.HasMeta(s, 0) })) > maxGlobSegments {
		sc.unknown = true
		return true
	}
	dirs := []string{sc.dir, sc.cwd}
	if root := segs[0]; root == "" || filepath.VolumeName(root) != "" {
		dirs, segs = []string{root + string(filepath.Separator)}, segs[1:]
	}
	if slices.Contains(segs, "**") {
		return sc.globstar(dirs, segs, r)
	}
	found := false
	for _, dir := range dirs {
		c := candidate{pat, dir, globbed}
		if _, seen := sc.examined[c]; seen {
			continue
		}
		sc.examined[c] = ""
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
				for _, e := range sc.readDir(d) {
					if rx.MatchString(e.Name()) && (i == len(segs)-1 || e.IsDir() || e.Type()&fs.ModeSymlink != 0) {
						next = append(next, filepath.Join(d, e.Name()))
					}
				}
			}
			at = next
		}
		for _, m := range at {
			if !sc.spend(1) {
				break
			}
			if info, err := os.Lstat(m); err == nil {
				found = true
				sc.reachEntry(m, info)
				sc.operand(m, info, r)
			}
		}
	}
	return found || sc.unknown
}

// unescape is a glob's segment with no wildcard as the name it matches.
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

// mayGlob reports whether w may hold a live wildcard: unquoted, as written or from a
// variable's value.
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

// slashed reports whether w, as written, may name a path whatever its expansions hold: it
// starts with ~, or holds a slash outside its substitutions.
func slashed(w *syntax.Word) bool {
	lead, ok := w.Parts[0].(*syntax.Lit)
	found := ok && strings.HasPrefix(lead.Value, "~")
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Lit:
			found = found || pathLike(n.Value)
		case *syntax.SglQuoted:
			found = found || pathLike(n.Value)
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp:
			return false // a slash in there is the substitution's own
		}
		return !found
	})
	return found
}

// fragmentSeparators are the bytes besides whitespace that separate fragments.
const fragmentSeparators = "'\"`()[]{}<>,;:@="

// isFragmentSeparator reports whether r separates fragments.
func isFragmentSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(fragmentSeparators, r)
}

// fragments are the words of text, a script's, a here-document's or an argument's, split at
// whitespace, quotes, brackets, parentheses, commas, :, @ and =, but a drive's colon (C:\x).
func fragments(text string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := -1
		for i, r := range text + " " {
			drive := r == ':' && start >= 0 && i == start+1 && i+1 < len(text) && (text[i+1] == '/' || text[i+1] == '\\') &&
				unicode.IsLetter(rune(text[start]))
			switch {
			case isFragmentSeparator(r) && !drive:
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

// shellValue is the part of the shell word w that may be a path: an option's value after
// its =, or a one-letter option's value written against it (-o/path), an assignment's
// value (NAME=/path), else w itself. An option with no value has none.
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

// isName reports whether s is a shell variable's name: letters, digits and underscores, not
// starting with a digit.
func isName(s string) bool {
	for i, r := range s {
		letter := r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return s != ""
}

// pathLike reports whether a shell word may name a path: it holds a slash, or on Windows a
// backslash, or is absolute (C:\x). A quoted Windows path keeps its backslashes.
func pathLike(w string) bool {
	return strings.ContainsRune(w, '/') || strings.ContainsRune(w, filepath.Separator) || filepath.IsAbs(w)
}
