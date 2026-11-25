package hookrun

import (
	"errors"
	"os"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// UnknownReach is a NUL-prefixed sentinel returned when a shell reach scan fails closed.
const UnknownReach, shellTime, maxShellPlaces, maxShellLookups, maxShellWords, maxShellStats, maxShellBytes, maxShellDepth, maxShellBraces, maxGlobSegments, substitutionSize, maxBelowDepth = "\x00unknown", 20 * time.Millisecond, 256, 4096, 16384, 131072, 1 << 20, 4, 16, 3, 4096, 4
const plain, program, target, folder, creates, data, part, slashedOnly, globbed, walked, physTarget role = 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10

var maxShellTime, errOpaque, unsetVar = shellTime, errors.New("an expansion ShellPaths cannot resolve"), new(string)

type role uint8

// scan holds the state of one bounded shell reach scan.
type scan struct {
	cwd, dir, prev, cwdRoot      string
	cwdRootDone, unknown, opaque bool
	none, out                    []string
	env                          func(string) (string, bool)
	vars                         map[string]*string
	cfg                          *expand.Config
	reads                        *cached
	places, created              map[string]bool
	examined                     map[candidate]cached
	work
	src, traps   string
	dirUncertain bool
	depth        int
	pushed       []string
	funcs        map[string]*syntax.Stmt
	feedBuf      *strings.Builder
	loopVars     map[string]bool
}
type work struct {
	lookups, stats, bytes, words int
	deadline                     time.Time
	noRecover                    bool
	historical                   bool
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

func unknownReach(command string) string { return UnknownReach + "\x00" + firstWords(command) }
func isUnknownReach(p string) bool       { return strings.HasPrefix(p, UnknownReach) }
func (r role) whole() bool               { return r == program || r == target || r == physTarget }
func (r role) needsSlash() bool          { return r == program || r == data || r == part || r == slashedOnly }
func newScan(cwd string, env func(string) (string, bool), done work) *scan {
	done.deadline = pick(done.deadline.IsZero(), time.Now().Add(maxShellTime), done.deadline)
	sc := &scan{cwd: cwd, dir: cwd, env: env, vars: map[string]*string{}, funcs: map[string]*syntax.Stmt{}, loopVars: map[string]bool{}, places: map[string]bool{}, created: map[string]bool{}, examined: map[candidate]cached{}, work: done}
	sc.cfg = sc.config()
	return sc
}
func (sc *scan) config() *expand.Config {
	return &expand.Config{Env: sc, CmdSubst: sc.substitute, ProcSubst: func(*syntax.ProcSubst) (string, error) { return "", errOpaque }, NoUnset: true}
}
func pick[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}
func historicalEnv(name string) (string, bool) {
	if name == "HOME" || (os.PathSeparator == '\\' && name == "USERPROFILE") {
		return os.LookupEnv(name)
	}
	if name == "PATH" && os.PathSeparator == '/' {
		return "/usr/bin:/bin", true
	}
	return "", false
}
func (sc *scan) isHistoricalUnknownVar(name string) bool {
	if !sc.historical || !isName(name) || name == "_" || name == "PWD" || name == "OLDPWD" || name == "IFS" || name == "HOME" || (os.PathSeparator == '/' && name == "PATH") || (os.PathSeparator == '\\' && name == "USERPROFILE") {
		return false
	}
	v, set := sc.vars[name]
	return !set || v == nil
}
func (sc *scan) Get(name string) expand.Variable {
	if strings.HasPrefix(name, "HOME ") || sc.isUnresolvedVar(name) || sc.isHistoricalUnknownVar(name) || !isName(name) || name == "_" {
		sc.opaque = true
		return expand.Variable{Set: true, Kind: expand.String}
	}
	v, ok := sc.lookup(name)
	return expand.Variable{Set: ok, Kind: pick(ok, expand.String, expand.Unknown), Str: v}
}
func (sc *scan) Each(func(string, expand.Variable) bool) { sc.opaque = true }
func (sc *scan) lookup(name string) (string, bool) {
	if name == "PWD" || name == "OLDPWD" && sc.prev != "" {
		return pick(name == "PWD", sc.dir, sc.prev), name != "PWD" || sc.dir != ""
	} else if v, ok := sc.vars[name]; !isName(name) || name == "_" || ok && (v == nil || v == unsetVar) {
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
func (sc *scan) spend(n int) bool        { return sc.budget(&sc.lookups, n, maxShellLookups) }
func (sc *scan) spendStat(n int) bool    { return sc.budget(&sc.stats, n, maxShellStats) }
func (sc *scan) charge(n int) bool       { return sc.budget(&sc.bytes, n, maxShellBytes) }
func (sc *scan) size(w *syntax.Word) int { return sc.sizeIn(w, false) }
func (sc *scan) sizeIn(w *syntax.Word, opaqueSubst bool) (n int) {
	if w != nil {
		syntax.Walk(w, func(node syntax.Node) bool {
			switch x := pick(n > maxShellBytes, syntax.Node(nil), node).(type) {
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
func (sc *scan) paramValue(pe *syntax.ParamExp) string {
	v, _ := sc.lookup(pick(pe.Param != nil, pe.Param, &syntax.Lit{}).Value)
	return pick(pe.Excl, sc.envVal(pick(pe.Excl, v, "")), v)
}
func (sc *scan) paramSize(pe *syntax.ParamExp, opaqueSubst bool) int {
	if pe.Index != nil || pe.Slice != nil || pe.Names != 0 {
		return maxShellBytes + 1
	}
	n := len(sc.paramValue(pe))
	if pe.Repl != nil {
		if orig := sc.sizeIn(pe.Repl.Orig, true); pe.Repl.Orig != nil && (orig > maxShellExpr || n > maxShellBytes/max(orig, 1)) {
			return maxShellBytes + 1
		} else if with := sc.sizeIn(pe.Repl.With, opaqueSubst || pe.Repl.All && n > 0); pe.Repl.With != nil {
			n = pick(with >= (maxShellBytes+1)/max(n, 1), maxShellBytes+1, max(n, 1)*(1+with))
		}
	}
	if pe.Exp != nil && pe.Exp.Word != nil {
		pat := isCaseOp(pe.Exp.Op) || pe.Exp.Op == syntax.RemSmallPrefix || pe.Exp.Op == syntax.RemLargePrefix || pe.Exp.Op == syntax.RemSmallSuffix || pe.Exp.Op == syntax.RemLargeSuffix
		w := sc.sizeIn(pe.Exp.Word, opaqueSubst || pat)
		n = pick(pat && (w > maxShellExpr || n > maxShellBytes/max(w, 1)), maxShellBytes+1, n+w)
	}
	return min(n, maxShellBytes+1)
}
func (sc *scan) ifs() string { v, ok := sc.lookup("IFS"); return pick(ok, v, " \t\n") }
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
			if rem = pick(isCase, rem-len(v), rem); !isCase {
				rem = sc.consumeFields(v, ifs, rem)
			}
			if rem >= 0 && pe.Exp != nil && pe.Exp.Word != nil {
				rem = sc.checkPartsFields(pe.Exp.Word.Parts, ifs, rem, true)
			}
			if before := rem; rem >= 0 && pe.Repl != nil && pe.Repl.With != nil {
				if rem = sc.checkPartsFields(pe.Repl.With.Parts, ifs, rem, true); rem >= 0 && pe.Repl.All && len(v) > 0 {
					rem = pick(len(v) > rem/(1+before-rem) && sc.fail(), -1, rem-len(v)*(1+before-rem))
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
	out, ifs, n, braced := []string(nil), sc.ifs(), 0, *w
	add := func(b *syntax.Word) bool {
		if len(ifs) > 64 && sc.fail() || !sc.charge(sc.size(b)) || ifs != "" && sc.checkPartsFields(b.Parts, ifs, maxShellWords-len(out), false) < 0 {
			return false
		}
		prev := sc.opaque
		sc.opaque = false
		f, err := expand.Fields(sc.cfg, b)
		if sc.opaque, prev = prev, sc.opaque; err != nil || prev || len(out)+len(f) > maxShellWords && sc.fail() {
			return false
		}
		out = append(out, f...)
		return true
	}
	for _, p := range w.Parts {
		if lit, ok := p.(*syntax.Lit); ok {
			n += strings.Count(lit.Value, "{")
		}
	}
	if n > maxShellBraces && sc.fail() {
		return nil, errOpaque
	} else if n == 0 || !syntax.SplitBraces(&braced) {
		ok := add(w)
		return pick(ok, out, nil), pick(ok, nil, errOpaque)
	}
	for b, err := range expand.BracesSeq(sc.cfg, &braced) {
		if (err != nil || !sc.spend(1)) && sc.fail() || !add(b) {
			return nil, errOpaque
		}
	}
	return out, nil
}
