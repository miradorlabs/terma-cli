package hookrun

import (
	"errors"
	"iter"
	"os"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// Bounds on a shell reach scan (maxShellTime is overridden to 1s in tests).
const (
	shellTime, maxShellPlaces, maxShellLookups, maxShellWords, maxShellStats                       = 20 * time.Millisecond, 256, 4096, 16384, 32768
	maxShellBytes, maxShellDepth, maxShellBraces, maxGlobSegments, substitutionSize, maxBelowDepth = 1 << 20, 4, 16, 3, 4096, 4
	UnknownReach                                                                                   = "\x00unknown" // NUL-prefixed so it cannot collide with a path
)

var maxShellTime, errOpaque = shellTime, errors.New("an expansion ShellPaths cannot resolve")

func unknownReach(command string) string { return UnknownReach + "\x00" + firstWords(command) }
func isUnknownReach(p string) bool       { return strings.HasPrefix(p, UnknownReach) }

type role uint8

const plain, program, target, folder, creates, data, part, slashedOnly, globbed, walked role = 0, 1, 2, 3, 4, 5, 6, 7, 8, 9

func (r role) whole() bool      { return r == program || r == target }
func (r role) needsSlash() bool { return r == program || r == data || r == part || r == slashedOnly }
func (r role) mayCreate(slashed bool) bool {
	return r == creates || slashed && (r == plain || r == folder || r == slashedOnly)
}
func (r role) mustResolve(slashed bool) bool {
	return r == target || r == folder || slashed && r != program && r != data && r != part
}

// scan holds the state of one bounded shell reach scan.
type scan struct {
	cwd, dir, prev, cwdRoot string
	cwdRootDone, unknown    bool
	none, out               []string
	env                     func(string) (string, bool)
	vars                    map[string]*string
	cfg                     *expand.Config
	reads                   *operands
	places                  map[string]bool
	examined                map[candidate]cached
	work
}
type work struct {
	lookups, stats, bytes, words int
	deadline                     time.Time
}
type candidate struct {
	word, dir string
	r         role
}
type cached struct {
	dir     string
	named   bool
	folders []string
}

func newScan(cwd string, env func(string) (string, bool), done work) *scan {
	if done.deadline.IsZero() {
		done.deadline = time.Now().Add(maxShellTime)
	}
	sc := &scan{cwd: cwd, dir: cwd, env: env, vars: map[string]*string{}, places: map[string]bool{}, examined: map[candidate]cached{}, work: done}
	sc.cfg = sc.config()
	return sc
}
func (sc *scan) config() *expand.Config {
	return &expand.Config{Env: sc, CmdSubst: sc.substitute, ProcSubst: func(*syntax.ProcSubst) (string, error) { return "", errOpaque }, NoUnset: true}
}
func (sc *scan) Get(name string) expand.Variable {
	if v, ok := sc.lookup(name); ok {
		return expand.Variable{Set: true, Kind: expand.String, Str: v}
	}
	return expand.Variable{}
}
func (sc *scan) Each(func(string, expand.Variable) bool) {}
func (sc *scan) lookup(name string) (string, bool) {
	if name == "PWD" {
		return sc.dir, sc.dir != ""
	} else if name == "OLDPWD" && sc.prev != "" {
		return sc.prev, true
	} else if v, ok := sc.vars[name]; ok && v == nil {
		return "", false
	} else if ok {
		return *v, true
	} else if v, ok := sc.env(name); ok || name != "HOME" {
		return v, ok
	}
	home, err := os.UserHomeDir()
	return home, err == nil
}
func (sc *scan) fail() bool { sc.unknown = true; return true }
func (sc *scan) place(p string) {
	if !sc.places[p] && (len(sc.places) < maxShellPlaces || !sc.fail()) {
		sc.places[p], sc.out = true, append(sc.out, p)
	}
}
func (sc *scan) budget(counter *int, n, limit int) bool {
	if sc.unknown || *counter+n > limit || time.Now().After(sc.deadline) {
		return !sc.fail()
	}
	*counter += n
	return true
}
func (sc *scan) spend(n int) bool       { return sc.budget(&sc.lookups, n, maxShellLookups) }
func (sc *scan) spendStat(n int) bool   { return sc.budget(&sc.stats, n, maxShellStats) }
func (sc *scan) charge(n int) bool      { return sc.budget(&sc.bytes, n, maxShellBytes) }
func (sc *scan) size(w syntax.Node) int { n, _ := sc.sizeIn(w, false, false); return n }
func (sc *scan) sizeIn(w syntax.Node, opaqueSubst, arith bool) (n int, unres bool) {
	if w != nil {
		syntax.Walk(w, func(node syntax.Node) bool {
			switch x := node.(type) {
			case *syntax.Lit:
				n += len(x.Value)
			case *syntax.SglQuoted:
				n += len(x.Value)
			case *syntax.ParamExp:
				pn, pu := sc.paramSize(x, opaqueSubst)
				n, unres = n+pn, unres || pu
				return false
			case *syntax.CmdSubst, *syntax.ProcSubst:
				if n += substitutionSize; opaqueSubst {
					n = maxShellBytes + 1
				}
				return false
			case *syntax.ArithmExp:
				an, au := sc.sizeIn(x.X, opaqueSubst, true)
				n, unres = n+substitutionSize+an, unres || au
				return false
			case *syntax.Word:
				unres = unres || arith && sc.isUnresolvedVar(x.Lit())
			}
			return n <= maxShellBytes
		})
	}
	return min(n, maxShellBytes+1), unres
}
func (sc *scan) paramValue(pe *syntax.ParamExp) (v string, unres bool) {
	if pe.Param != nil {
		v, _ = sc.lookup(pe.Param.Value)
		if unres = sc.isUnresolvedVar(pe.Param.Value); pe.Excl {
			unres = unres || sc.isUnresolvedVar(v)
			v, _ = sc.lookup(v)
		}
	}
	return v, unres
}
func (sc *scan) paramSize(pe *syntax.ParamExp, opaqueSubst bool) (int, bool) {
	v, unres := sc.paramValue(pe)
	n := len(v)
	if pe.Repl != nil && pe.Repl.Orig != nil {
		orig, ou := sc.sizeIn(pe.Repl.Orig, true, false)
		if unres = unres || ou; orig > maxShellExpr || n > maxShellBytes/max(orig, 1) {
			return maxShellBytes + 1, unres
		}
	}
	if pe.Repl != nil && pe.Repl.With != nil {
		with, wu := sc.sizeIn(pe.Repl.With, opaqueSubst || pe.Repl.All && n > 0, false)
		if unres = unres || wu; with >= (maxShellBytes+1)/max(n, 1) {
			return maxShellBytes + 1, unres
		}
		n = max(n, 1) * (1 + with)
	}
	if pe.Exp != nil && pe.Exp.Word != nil {
		pat := pe.Exp.Op == syntax.RemSmallPrefix || pe.Exp.Op == syntax.RemLargePrefix || pe.Exp.Op == syntax.RemSmallSuffix || pe.Exp.Op == syntax.RemLargeSuffix || pe.Exp.Op == syntax.UpperFirst || pe.Exp.Op == syntax.UpperAll || pe.Exp.Op == syntax.LowerFirst || pe.Exp.Op == syntax.LowerAll
		w, wu := sc.sizeIn(pe.Exp.Word, opaqueSubst || pat, false)
		if unres = unres || wu; pat && (w > maxShellExpr || n > maxShellBytes/max(w, 1)) {
			return maxShellBytes + 1, unres
		}
		n += w
	}
	in, iu := sc.sizeIn(pe.Index, opaqueSubst, true)
	if n, unres = n+in, unres || iu; pe.Slice != nil {
		on, ou := sc.sizeIn(pe.Slice.Offset, opaqueSubst, true)
		ln, lu := sc.sizeIn(pe.Slice.Length, opaqueSubst, true)
		n, unres = n+on+ln, unres || ou || lu
	}
	return min(n, maxShellBytes+1), unres
}
func (sc *scan) ifs() string {
	if ifs, ok := sc.lookup("IFS"); ok {
		return ifs
	}
	return " \t\n"
}
func (sc *scan) chargeFields(v string) bool {
	ifs := sc.ifs()
	return ifs == "" || (len(ifs) <= 64 || !sc.fail()) && sc.budget(&sc.words, countFields(v, ifs, maxShellWords-sc.words), maxShellWords)
}
func (sc *scan) checkPartsFields(parts []syntax.WordPart, ifs string, rem int, inParam bool) int {
	for i := 0; i < len(parts) && rem >= 0; i++ {
		if pe, ok := parts[i].(*syntax.ParamExp); ok {
			v, _ := sc.paramValue(pe)
			if (pe.Exp != nil && pe.Exp.Op == syntax.OtherParamOps || pe.Repl != nil && pe.Repl.All && len(v) > rem) && sc.fail() {
				return -1
			}
			if rem = sc.consumeFields(v, ifs, rem); rem >= 0 && pe.Exp != nil && pe.Exp.Word != nil {
				rem = sc.checkPartsFields(pe.Exp.Word.Parts, ifs, rem, true)
			}
			if rem >= 0 && pe.Repl != nil && pe.Repl.With != nil {
				before := rem
				if rem = sc.checkPartsFields(pe.Repl.With.Parts, ifs, rem, true); rem >= 0 && pe.Repl.All && len(v) > 0 {
					if per := 1 + before - rem; len(v) > rem/per && sc.fail() {
						return -1
					}
					rem -= len(v) * (1 + before - rem)
				}
			}
		} else if inParam {
			if p, ok := parts[i].(*syntax.DblQuoted); ok {
				rem = sc.checkPartsFields(p.Parts, ifs, rem, true)
			} else if p, ok := parts[i].(*syntax.Lit); ok {
				rem = sc.consumeFields(p.Value, ifs, rem)
			} else if p, ok := parts[i].(*syntax.SglQuoted); ok {
				rem = sc.consumeFields(p.Value, ifs, rem)
			}
		}
	}
	return rem
}
func (sc *scan) consumeFields(v, ifs string, rem int) int {
	n := countFields(v, ifs, rem)
	if n > rem && sc.fail() {
		return -1
	}
	return rem - max(n-1, 0)
}
func countFields(v, ifs string, limit int) int {
	var ascii [128]bool
	for _, r := range ifs {
		if r < 128 {
			ascii[r] = true
		}
	}
	n, inWord := 0, false
	for _, r := range v {
		sep := r < 128 && ascii[r] || r >= 128 && strings.ContainsRune(ifs, r)
		if !inWord && (!sep || r != ' ' && r != '\t' && r != '\n') {
			n++
		}
		if inWord = !sep; n > limit {
			return n
		}
	}
	return n
}
func (sc *scan) fields(w *syntax.Word) ([]string, error) {
	var out []string
	for b, err := range sc.braces(w) {
		if err != nil {
			return nil, errOpaque
		} else if n, unres := sc.sizeIn(b, false, false); len(sc.ifs()) > 64 && sc.fail() || !sc.charge(n) || sc.ifs() != "" && sc.checkPartsFields(b.Parts, sc.ifs(), maxShellWords-len(out), false) < 0 || unres {
			return nil, errOpaque
		} else if f, err := expand.Fields(sc.cfg, b); err != nil || len(out)+len(f) > maxShellWords && sc.fail() {
			return nil, errOpaque
		} else {
			out = append(out, f...)
		}
	}
	return out, nil
}
func (sc *scan) braces(w *syntax.Word) iter.Seq2[*syntax.Word, error] {
	return func(yield func(*syntax.Word, error) bool) {
		n := 0
		for _, p := range w.Parts {
			if lit, ok := p.(*syntax.Lit); ok {
				n += strings.Count(lit.Value, "{")
			}
		}
		if braced := *w; n > maxShellBraces && sc.fail() {
			yield(nil, errOpaque)
		} else if n == 0 || !syntax.SplitBraces(&braced) {
			yield(w, nil)
		} else {
			for b, err := range expand.BracesSeq(sc.cfg, &braced) {
				if (err != nil || !sc.spend(1)) && sc.fail() {
					b, err = nil, errOpaque
				}
				if !yield(b, err) || err != nil {
					return
				}
			}
		}
	}
}
