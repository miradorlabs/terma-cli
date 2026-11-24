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
	shellTime, maxShellPlaces, maxShellLookups, maxShellWords = 20 * time.Millisecond, 256, 4096, 16384
	maxShellBytes, maxShellDepth, maxShellBraces              = 1 << 20, 4, 16
	maxGlobSegments, substitutionSize                         = 3, 4096
	UnknownReach                                              = "\x00unknown" // NUL-prefixed so it cannot collide with a path
)

var (
	maxShellTime = shellTime
	errOpaque    = errors.New("an expansion ShellPaths cannot resolve")
)

func unknownReach(command string) string { return UnknownReach + "\x00" + firstWords(command) }
func isUnknownReach(p string) bool       { return strings.HasPrefix(p, UnknownReach) }

type role uint8

const (
	plain role = iota // argument: path if present, or new file if slashed
	program
	target
	folder
	creates
	data
	part
	slashedOnly
	globbed
	walked
)

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
	cwd, dir, prev, cwdRoot                     string
	cwdRootDone, cwdChecked, cwdRemote, unknown bool
	none, out                                   []string
	env                                         func(string) (string, bool)
	vars                                        map[string]*string
	cfg                                         *expand.Config
	reads                                       *operands
	places                                      map[string]bool
	examined                                    map[candidate]cached
	work
}
type work struct {
	lookups, bytes, words int
	deadline              time.Time
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
func (sc *scan) place(p string) {
	if sc.places[p] {
		return
	}
	if len(sc.places) == maxShellPlaces {
		sc.unknown = true
		return
	}
	sc.places[p], sc.out = true, append(sc.out, p)
}
func (sc *scan) budget(counter *int, n, limit int) bool {
	if sc.unknown || *counter+n > limit || time.Now().After(sc.deadline) {
		sc.unknown = true
		return false
	}
	*counter += n
	return true
}
func (sc *scan) spend(n int) bool  { return sc.budget(&sc.lookups, n, maxShellLookups) }
func (sc *scan) charge(n int) bool { return sc.budget(&sc.bytes, n, maxShellBytes) }
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
func (sc *scan) paramValue(pe *syntax.ParamExp) string {
	if pe.Param == nil {
		return ""
	}
	v, _ := sc.lookup(pe.Param.Value)
	if pe.Excl {
		v, _ = sc.lookup(v)
	}
	return v
}
func (sc *scan) paramSize(pe *syntax.ParamExp) int {
	n := len(sc.paramValue(pe))
	if pe.Repl != nil && pe.Repl.With != nil {
		base := max(n, 1)
		if with := sc.size(pe.Repl.With); with >= (maxShellBytes+1)/base {
			n = maxShellBytes + 1
		} else {
			n = base * (1 + with)
		}
	}
	if pe.Exp != nil && pe.Exp.Word != nil {
		n += sc.size(pe.Exp.Word)
	}
	return min(n, maxShellBytes+1)
}
func (sc *scan) ifs() string {
	if ifs, ok := sc.lookup("IFS"); ok {
		return ifs
	}
	return " \t\n"
}
func (sc *scan) fieldsBounded(w *syntax.Word, limit int) bool {
	ifs := sc.ifs()
	return ifs == "" || sc.checkPartsFields(w.Parts, ifs, limit, false, false) >= 0
}
func (sc *scan) chargeFields(v string) bool {
	if ifs := sc.ifs(); ifs != "" {
		return sc.budget(&sc.words, countFields(v, ifs, maxShellWords-sc.words), maxShellWords)
	}
	return true
}
func (sc *scan) checkPartsFields(parts []syntax.WordPart, ifs string, rem int, inParam, noSubst bool) int {
	for i := 0; i < len(parts) && rem >= 0; i++ {
		if pe, ok := parts[i].(*syntax.ParamExp); ok {
			v := sc.paramValue(pe)
			if pe.Repl != nil && pe.Repl.All && len(v) > rem {
				sc.unknown = true
				return -1
			}
			if rem = sc.consumeFields(v, ifs, rem); rem >= 0 && pe.Exp != nil && pe.Exp.Word != nil {
				rem = sc.checkPartsFields(pe.Exp.Word.Parts, ifs, rem, true, noSubst)
			}
			if rem >= 0 && pe.Repl != nil && pe.Repl.With != nil {
				before := rem
				if rem = sc.checkPartsFields(pe.Repl.With.Parts, ifs, rem, true, noSubst || pe.Repl.All && len(v) > 1); rem >= 0 && pe.Repl.All && len(v) > 0 {
					if per := 1 + (before - rem); len(v) > rem/per {
						sc.unknown = true
						return -1
					} else {
						rem -= len(v) * per
					}
				}
			}
		} else if inParam {
			switch p := parts[i].(type) {
			case *syntax.DblQuoted:
				rem = sc.checkPartsFields(p.Parts, ifs, rem, true, noSubst)
			case *syntax.Lit:
				rem = sc.consumeFields(p.Value, ifs, rem)
			case *syntax.SglQuoted:
				rem = sc.consumeFields(p.Value, ifs, rem)
			case *syntax.CmdSubst:
				if noSubst {
					sc.unknown = true
					return -1
				}
			}
		}
	}
	return rem
}
func (sc *scan) consumeFields(v, ifs string, rem int) int {
	if n := countFields(v, ifs, rem); n > rem {
		sc.unknown = true
		return -1
	} else if n > 1 {
		return rem - (n - 1)
	}
	return rem
}
func countFields(v, ifs string, limit int) int {
	n, inWord := 0, false
	for _, r := range v {
		if !strings.ContainsRune(ifs, r) {
			if !inWord {
				inWord, n = true, n+1
			}
		} else {
			if !inWord && r != ' ' && r != '\t' && r != '\n' {
				n++
			}
			inWord = false
		}
		if n > limit {
			return n
		}
	}
	return n
}
func (sc *scan) fields(w *syntax.Word) ([]string, error) {
	var out []string
	for b, err := range sc.braces(w) {
		if err != nil || !sc.charge(sc.size(b)) || !sc.fieldsBounded(b, maxShellWords-len(out)) {
			return nil, errOpaque
		}
		f, err := expand.Fields(sc.cfg, b)
		if err != nil {
			return nil, err
		}
		if out = append(out, f...); len(out) > maxShellWords {
			sc.unknown = true
			return nil, errOpaque
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
		if braced := *w; n > maxShellBraces {
			sc.unknown = true
			yield(nil, errOpaque)
		} else if n == 0 || !syntax.SplitBraces(&braced) {
			yield(w, nil)
		} else {
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
