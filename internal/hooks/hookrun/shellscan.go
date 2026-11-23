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
	shellTime       = 20 * time.Millisecond
	maxShellPlaces  = 256
	maxShellLookups = 4096
	maxShellWords   = 4 * maxShellLookups
	maxShellBytes   = 1 << 20
	maxShellDepth   = 4
	maxShellBraces  = 16
	maxGlobSegments = 3
)

var maxShellTime = shellTime

// UnknownReach marks an unresolved reach (NUL-prefixed so it cannot collide with a path).
const UnknownReach = "\x00unknown"

func unknownReach(command string) string { return UnknownReach + "\x00" + firstWords(command) }
func isUnknownReach(p string) bool       { return strings.HasPrefix(p, UnknownReach) }

var errOpaque = errors.New("an expansion ShellPaths cannot resolve")

type role uint8

const (
	plain       role = iota // argument: path if present, or new file if slashed
	program                 // program name: reach only when slashed and present
	target                  // cd target: unknown when unresolved
	folder                  // folder option (-C, --git-dir): unknown when unresolved
	creates                 // > target: nearest existing folder when absent
	data                    // heredoc or whitespace-holding prose: only when slashed and present
	part                    // colon/comma fragment (-v ../x:/src): slashed and present, operand
	slashedOnly             // assignment value or inline script word: only when slashed
	globbed                 // glob pattern cache key
	walked                  // folder-below walk cache key
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
	cwd, dir, prev string
	cwdRoot        string
	cwdRootDone    bool
	none           []string
	env            func(string) (string, bool)
	vars           map[string]*string
	cfg            *expand.Config
	reads          *operands
	out            []string
	places         map[string]bool
	examined       map[candidate]cached
	work
	unknown bool
}

type work struct {
	lookups, bytes int
	deadline       time.Time
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
	return &expand.Config{Env: sc, CmdSubst: sc.substitute, ProcSubst: opaqueProcess, NoUnset: true}
}

// Get resolves a shell variable for expand (unset fails under NoUnset unless a fallback is given).
func (sc *scan) Get(name string) expand.Variable {
	if v, ok := sc.lookup(name); ok {
		return expand.Variable{Set: true, Kind: expand.String, Str: v}
	}
	return expand.Variable{}
}

// Each lists no variables: expand asks for each it expands.
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
	switch {
	case sc.places[p]:
	case len(sc.places) == maxShellPlaces:
		sc.unknown = true
	default:
		sc.places[p] = true
		sc.out = append(sc.out, p)
	}
}

func (sc *scan) spend(n int) bool {
	if sc.unknown || sc.lookups+n > maxShellLookups || time.Now().After(sc.deadline) {
		sc.unknown = true
		return false
	}
	sc.lookups += n
	return true
}

func (sc *scan) charge(n int) bool {
	if sc.unknown || sc.bytes+n > maxShellBytes || time.Now().After(sc.deadline) {
		sc.unknown = true
		return false
	}
	sc.bytes += n
	return true
}

const substitutionSize = 4096

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
	var v string
	if pe.Param != nil {
		v, _ = sc.lookup(pe.Param.Value)
		if pe.Excl {
			v, _ = sc.lookup(v)
		}
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

// fieldsBounded ensures unquoted parameter expansions cannot split into > maxShellWords fields
// before expand.Fields materializes them.
func (sc *scan) fieldsBounded(w *syntax.Word, limit int) bool {
	ifs, ok := sc.lookup("IFS")
	if !ok {
		ifs = " \t\n"
	}
	if ifs == "" {
		return true
	}
	return sc.checkPartsFields(w.Parts, ifs, limit, false) >= 0
}

func (sc *scan) checkPartsFields(parts []syntax.WordPart, ifs string, rem int, inParam bool) int {
	for _, p := range parts {
		switch p := p.(type) {
		case *syntax.DblQuoted:
			if inParam {
				rem = sc.checkPartsFields(p.Parts, ifs, rem, true)
			}
		case *syntax.Lit:
			if inParam {
				rem = sc.consumeFields(p.Value, ifs, rem)
			}
		case *syntax.SglQuoted:
			if inParam {
				rem = sc.consumeFields(p.Value, ifs, rem)
			}
		case *syntax.ParamExp:
			v := sc.paramValue(p)
			if p.Repl != nil && p.Repl.All && len(v) > rem {
				sc.unknown = true
				return -1
			}
			if rem = sc.consumeFields(v, ifs, rem); rem >= 0 && p.Exp != nil && p.Exp.Word != nil {
				rem = sc.checkPartsFields(p.Exp.Word.Parts, ifs, rem, true)
			}
			if rem >= 0 && p.Repl != nil && p.Repl.With != nil {
				before := rem
				rem = sc.checkPartsFields(p.Repl.With.Parts, ifs, rem, true)
				if rem >= 0 && p.Repl.All && len(v) > 0 {
					perMatch := 1 + (before - rem)
					if len(v) > rem/perMatch {
						sc.unknown = true
						return -1
					}
					rem -= len(v) * perMatch
				}
			}
		}
		if rem < 0 {
			return -1
		}
	}
	return rem
}

func (sc *scan) consumeFields(v, ifs string, rem int) int {
	if n := countFields(v, ifs, rem); n > rem {
		sc.unknown = true
		return -1
	} else if n > 1 {
		rem -= n - 1
	}
	return rem
}

func countFields(v, ifs string, limit int) int {
	n, inWord := 0, false
	for _, r := range v {
		if strings.ContainsRune(ifs, r) {
			if !inWord && r != ' ' && r != '\t' && r != '\n' {
				if n++; n > limit {
					return n
				}
			}
			inWord = false
		} else if !inWord {
			inWord = true
			if n++; n > limit {
				return n
			}
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
		out = append(out, f...)
		if len(out) > maxShellWords {
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
