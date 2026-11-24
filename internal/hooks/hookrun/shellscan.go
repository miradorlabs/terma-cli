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
	shellTime, maxShellPlaces, maxShellLookups, maxShellWords, maxShellStats                       = 20 * time.Millisecond, 256, 4096, 16384, 131072
	maxShellBytes, maxShellDepth, maxShellBraces, maxGlobSegments, substitutionSize, maxBelowDepth = 1 << 20, 4, 16, 3, 4096, 4
	UnknownReach                                                                                   = "\x00unknown" // NUL-prefixed so it cannot collide with a path
)

var maxShellTime, errOpaque = shellTime, errors.New("an expansion ShellPaths cannot resolve")

func unknownReach(command string) string { return UnknownReach + "\x00" + firstWords(command) }
func isUnknownReach(p string) bool       { return strings.HasPrefix(p, UnknownReach) }

type role uint8

const plain, program, target, folder, creates, data, part, slashedOnly, globbed, walked, physTarget role = 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10

func (r role) whole() bool      { return r == program || r == target || r == physTarget }
func (r role) needsSlash() bool { return r == program || r == data || r == part || r == slashedOnly }
func (r role) mayCreate(slashed bool) bool {
	return r == creates || slashed && (r == plain || r == folder || r == slashedOnly)
}
func (r role) mustResolve(slashed bool) bool {
	return r == target || r == physTarget || r == folder || slashed && r != program && r != data && r != part
}

// scan holds the state of one bounded shell reach scan.
type scan struct {
	cwd, dir, prev, cwdRoot      string
	cwdRootDone, unknown, opaque bool
	none, out                    []string
	env                          func(string) (string, bool)
	vars                         map[string]*string
	cfg                          *expand.Config
	reads                        *operands
	places                       map[string]bool
	examined                     map[candidate]cached
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
func pick[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
func (sc *scan) Get(name string) expand.Variable {
	if strings.HasPrefix(name, "HOME ") || sc.isUnresolvedVar(name) || !isName(name) || name == "_" {
		sc.opaque = true
		return expand.Variable{Set: true, Kind: expand.String}
	}
	v, ok := sc.lookup(name)
	return expand.Variable{Set: ok, Kind: pick(ok, expand.String, expand.Unknown), Str: v}
}
func (sc *scan) Each(func(string, expand.Variable) bool) {}
func (sc *scan) lookup(name string) (string, bool) {
	if name == "PWD" {
		return sc.dir, sc.dir != ""
	} else if name == "OLDPWD" && sc.prev != "" {
		return sc.prev, true
	} else if v, ok := sc.vars[name]; !isName(name) || name == "_" || ok && v == nil {
		return "", false
	} else if ok {
		return *v, true
	}
	return sc.env(name)
}
func (sc *scan) homeDir() (string, bool) {
	if home, ok := sc.lookup("HOME"); ok || sc.isUnresolvedVar("HOME") {
		return home, ok
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
func (sc *scan) size(w syntax.Node) int { return sc.sizeIn(w, false) }
func (sc *scan) sizeIn(w syntax.Node, opaqueSubst bool) (n int) {
	if w != nil {
		syntax.Walk(w, func(node syntax.Node) bool {
			switch x := node.(type) {
			case *syntax.Lit:
				n += len(x.Value)
			case *syntax.SglQuoted:
				n += len(x.Value)
			case *syntax.ParamExp:
				n += sc.paramSize(x, opaqueSubst)
				return false
			case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp:
				_, arith := x.(*syntax.ArithmExp)
				n = pick(opaqueSubst || arith, maxShellBytes+1, n+substitutionSize)
				return false
			}
			return n <= maxShellBytes
		})
	}
	return min(n, maxShellBytes+1)
}
func isCaseOp(op syntax.ParExpOperator) bool {
	return op == syntax.UpperFirst || op == syntax.UpperAll || op == syntax.LowerFirst || op == syntax.LowerAll
}
func (sc *scan) paramValue(pe *syntax.ParamExp) (v string) {
	if pe.Param != nil {
		if v, _ = sc.lookup(pe.Param.Value); pe.Excl {
			v, _ = sc.lookup(v)
		}
	}
	return v
}
func (sc *scan) paramSize(pe *syntax.ParamExp, opaqueSubst bool) int {
	if pe.Index != nil || pe.Slice != nil {
		return maxShellBytes + 1
	}
	n := len(sc.paramValue(pe))
	if pe.Repl != nil && pe.Repl.Orig != nil {
		if orig := sc.sizeIn(pe.Repl.Orig, true); orig > maxShellExpr || n > maxShellBytes/max(orig, 1) {
			return maxShellBytes + 1
		}
	}
	if pe.Repl != nil && pe.Repl.With != nil {
		with := sc.sizeIn(pe.Repl.With, opaqueSubst || pe.Repl.All && n > 0)
		if with >= (maxShellBytes+1)/max(n, 1) {
			return maxShellBytes + 1
		}
		n = max(n, 1) * (1 + with)
	}
	if pe.Exp != nil && pe.Exp.Word != nil {
		pat := isCaseOp(pe.Exp.Op) || pe.Exp.Op == syntax.RemSmallPrefix || pe.Exp.Op == syntax.RemLargePrefix || pe.Exp.Op == syntax.RemSmallSuffix || pe.Exp.Op == syntax.RemLargeSuffix
		w := sc.sizeIn(pe.Exp.Word, opaqueSubst || pat)
		if pat && (w > maxShellExpr || n > maxShellBytes/max(w, 1)) {
			return maxShellBytes + 1
		}
		n += w
	}
	return min(n, maxShellBytes+1)
}
func (sc *scan) ifs() string {
	ifs, ok := sc.lookup("IFS")
	return pick(ok, ifs, " \t\n")
}
func (sc *scan) chargeFields(v string) bool {
	ifs := sc.ifs()
	return !sc.isUnresolvedVar("IFS") && (ifs == "" || (len(ifs) <= 64 || !sc.fail()) && sc.budget(&sc.words, countFields(v, ifs, maxShellWords-sc.words), maxShellWords))
}
func (sc *scan) checkPartsFields(parts []syntax.WordPart, ifs string, rem int, inParam bool) int {
	for i := 0; i < len(parts) && rem >= 0; i++ {
		if pe, ok := parts[i].(*syntax.ParamExp); ok {
			v, isCase := sc.paramValue(pe), pe.Exp != nil && isCaseOp(pe.Exp.Op)
			if (pe.Exp != nil && pe.Exp.Op == syntax.OtherParamOps || (isCase || pe.Repl != nil && pe.Repl.All) && len(v) > rem) && sc.fail() {
				return -1
			}
			if isCase {
				rem -= len(v)
			} else if rem = sc.consumeFields(v, ifs, rem); rem < 0 {
				return -1
			}
			if pe.Exp != nil && pe.Exp.Word != nil {
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
			switch p := parts[i].(type) {
			case *syntax.DblQuoted:
				rem = sc.checkPartsFields(p.Parts, ifs, rem, true)
			case *syntax.Lit:
				rem = sc.consumeFields(p.Value, ifs, rem)
			case *syntax.SglQuoted:
				rem = sc.consumeFields(p.Value, ifs, rem)
			}
		}
	}
	return rem
}
func (sc *scan) consumeFields(v, ifs string, rem int) int {
	n := countFields(v, ifs, rem)
	return pick(n > rem && sc.fail(), -1, rem-max(n-1, 0))
}
func countFields(v, ifs string, limit int) int {
	n, inWord := 0, false
	for _, r := range v {
		sep := strings.ContainsRune(ifs, r)
		n += pick(!inWord && (!sep || r != ' ' && r != '\t' && r != '\n'), 1, 0)
		if inWord = !sep; n > limit {
			return n
		}
	}
	return n
}
func (sc *scan) fields(w *syntax.Word) ([]string, error) {
	var out []string
	for b, err := range sc.braces(w) {
		if ifs := sc.ifs(); err != nil || len(ifs) > 64 && sc.fail() || !sc.charge(sc.size(b)) || ifs != "" && sc.checkPartsFields(b.Parts, ifs, maxShellWords-len(out), false) < 0 {
			return nil, errOpaque
		}
		prev := sc.opaque
		sc.opaque = false
		f, err := expand.Fields(sc.cfg, b)
		bad := sc.opaque
		if sc.opaque = prev; err != nil || bad || len(out)+len(f) > maxShellWords && sc.fail() {
			return nil, errOpaque
		}
		out = append(out, f...)
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
