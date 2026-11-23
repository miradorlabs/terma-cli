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

// Bounds on a shell reach scan: maxShellPlaces places returned, maxShellLookups filesystem
// lookups, maxShellWords words expanded, maxShellBytes bytes parsed/expanded across nested
// scripts, maxShellDepth nested scripts, maxShellBraces braces per word, and maxShellTime
// deadline (overridden to 1s in tests; deadline tests use shellTime directly).
const shellTime = 20 * time.Millisecond

var maxShellTime = shellTime

const (
	maxShellPlaces  = 256
	maxShellLookups = 4096
	maxShellWords   = 4 * maxShellLookups
	maxShellBytes   = 1 << 20
	maxShellDepth   = 4
	maxShellBraces  = 16
	maxGlobSegments = 3
)

// UnknownReach marks that a call may have reached an unresolved checkout (contains NUL so it
// never collides with a filesystem path); unknownReach appends NUL + firstWords(command).
const UnknownReach = "\x00unknown"

func unknownReach(command string) string { return UnknownReach + "\x00" + firstWords(command) }

func isUnknownReach(p string) bool { return strings.HasPrefix(p, UnknownReach) }

// errOpaque is returned when an expansion cannot be resolved statically.
var errOpaque = errors.New("an expansion ShellPaths cannot resolve")

// role is how a word counts toward a command's reach.
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

// work tracks lookup, byte, and deadline budgets across nested scans.
type work struct {
	lookups, bytes int
	deadline       time.Time
}

// candidate is a word as expanded, from where it was read and in what role, once looked at.
type candidate struct {
	word, dir string
	r         role
}

// cached records what a candidate lookup found, so a cache hit still replays its folder
// operands onto the current command's sc.reads (for example `ls .. && rg TODO ..`).
type cached struct {
	dir     string
	named   bool
	folders []string
}

// newScan is a scan from cwd, with done done, by maxShellTime from now when done has no
// deadline.
func newScan(cwd string, env func(string) (string, bool), done work) *scan {
	if done.deadline.IsZero() {
		done.deadline = time.Now().Add(maxShellTime)
	}
	sc := &scan{
		cwd: cwd, dir: cwd, env: env,
		vars:     map[string]*string{},
		places:   map[string]bool{},
		examined: map[candidate]cached{},
		work:     done,
	}
	sc.cfg = sc.config()
	return sc
}

// config is a configuration to expand words where the command is.
func (sc *scan) config() *expand.Config {
	return &expand.Config{Env: sc, CmdSubst: sc.substitute, ProcSubst: opaqueProcess, NoUnset: true}
}

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

// place adds the folder p to what the command reaches, past maxShellPlaces unknown.
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

// spend counts n lookups and reports whether the scan may make them: not past
// maxShellLookups or its deadline, which make the reach unknown.
func (sc *scan) spend(n int) bool {
	if sc.unknown || sc.lookups+n > maxShellLookups || time.Now().After(sc.deadline) {
		sc.unknown = true
		return false
	}
	sc.lookups += n
	return true
}

// charge counts n bytes parsed or expanded and reports whether the scan may: not past
// maxShellBytes or its deadline, which make the reach unknown.
func (sc *scan) charge(n int) bool {
	if sc.unknown || sc.bytes+n > maxShellBytes || time.Now().After(sc.deadline) {
		sc.unknown = true
		return false
	}
	sc.bytes += n
	return true
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
