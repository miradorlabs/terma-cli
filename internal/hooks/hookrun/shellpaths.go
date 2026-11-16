package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// Bounds on a ShellPaths scan, past any of which the reach is not known. maxShellPlaces
// bounds the folders it returns, each costing Classify a checkout lookup (some 20µs).
// maxShellLookups bounds its filesystem work, about 2µs a lookup: a stat of each distinct
// candidate, each entry a glob or a walk below a folder reads, each word braces make, and
// each pass of a for loop, charged its body's statements. maxShellWords bounds the words it
// expands, about 1µs each, those that cost no lookup included; maxShellBytes the bytes it
// parses and expands, all told, each script charged before it is parsed and each word what
// it may expand to before it is (size): a megabyte of short words parses in some 15ms, so
// the bound is one, however the bytes are split among scripts (bash -c) and expansions;
// maxShellDepth the scripts it reads inside each other; maxShellBraces the braces a word
// holds, as expand takes time and memory that grow with their square to make each word of a
// brace expansion (a word of 4000 took 400 MiB). maxShellTime bounds the scan's wall-clock
// time, all its readings told, checked at each lookup: a walk below a folder whose entries are
// not cached (a cold home folder took 4.5s within maxShellLookups) ends there, past it by one
// filesystem call at most. No command an agent runs comes near them, and they keep a
// pathological one within the hook's budget. In a test binary, whose tests share a machine
// with every other's, a scan may take far longer: there maxShellTime is a second, and the
// tests that hold a scan to a hook's deadline (shellTime) give it that.
const shellTime = 20 * time.Millisecond

var maxShellTime = shellTime

func init() {
	if testing.Testing() {
		maxShellTime = time.Second
	}
}

const (
	maxShellPlaces  = 256
	maxShellLookups = 4096
	maxShellWords   = 4 * maxShellLookups
	maxShellBytes   = 1 << 20
	maxShellDepth   = 4
	maxShellBraces  = 16
)

// UnknownReach is a path Classify takes to say the call may have reached a checkout no path
// names: a shell command reaching more places than ShellPaths returns, or a path-like word
// whose expansion it cannot resolve. It is no path (it holds a NUL), and the run turns
// Mixed, as for a checkout of an organization not known. ShellPaths follows it with a NUL
// and the command's first words (unknownReach), which the run ledger keeps as where.
const UnknownReach = "\x00unknown"

// unknownReach is UnknownReach naming command by its first words.
func unknownReach(command string) string { return UnknownReach + "\x00" + firstWords(command) }

// isUnknownReach reports whether p is UnknownReach, naming a command or not.
func isUnknownReach(p string) bool { return strings.HasPrefix(p, UnknownReach) }

// ShellPaths are the places a shell command may reach, for Classify, which is to take each
// alone (Alone): the folders of the existing paths it names (a folder its own place, and a
// symlink's target's as well), with UnknownReach when it may reach one it cannot name. It
// parses the command as bash does (mvdan.cc/sh) and reads each simple command's arguments,
// a word with no slash included when something is there and an option's or assignment's
// value (--flag=X, -oX, NAME=X) as the word; the fragments of every argument and assignment
// value, split at quotes, brackets, :, @, = and the like (-v ../x:/src, -d @../x,
// PYTHONPATH=.:../x), those of prose (git commit -m "fix a/b") only when slashed and there,
// a URL naming none but a file: URL its path (https://x/y names no //x/y: urlPath);
// its name, past wrappers (sudo, timeout 60, env A=1, xargs, uv run: the wrappers table),
// only as a path that is there (../other/x); its redirects' targets; its assignments'
// slashed values; a cd's, pushd's and popd's move, CDPATH's included, which later words
// resolve from as well as the cwd, until a subshell holding it ends, and -C's for tar, make
// and git; a for loop's body once for each of its words; as commands, the script of a shell's
// -c wherever it is among a command's words, past any wrapper, known or not (direnv exec .
// bash -c, strace -f sh -c, su -c), and a here-document, or echo's or printf's words, fed to
// a shell (any other output a shell reads is not known); and the slashed words of python -c,
// node -e and the like and of any other here-document, which reach only what is there. A
// slashed word naming nothing yet may be a file the command creates (touch, > new.md), so it
// reaches its nearest existing folder; a cd to nowhere reaches nothing. A program reads
// below each folder its words name, or below where it runs when they name nothing there
// (cd .. && pytest), unless it is known not to (the shallow table: ls without -R, git but
// git grep, echo...): a folder outside any checkout so read reaches each checkout below it
// (below), as deep as the program's depth option goes (find -maxdepth), and so does the
// folder a glob's ** segment reads (zsh's).
//
// Words are expanded first (mvdan.cc/sh/v3/expand): braces, variables from the command's
// assignments, its for loops and the hook's environment, $PWD and $(pwd) where the command
// is, the few command substitutions whose output is known (outputs), and globs, walked a
// segment at a time. A word whose expansion cannot be resolved (another command
// substitution's output, an unset variable) is UnknownReach when it may be a path: it is
// slashed, a cd's target, or a folder option's value (-C, --git-dir, --dir...). So is such a
// word on another machine's share (//host/share, \\host\share), as no lookup touches a path
// that starts with two separators, which Windows sends to the network (remote), but on the
// share the command runs in; and so is a command past any of the scan's bounds or
// maxGlobSegments, or its deadline. A command that does not parse runs up to its error, as
// bash and zsh do: the statements before it are read as any are, and the rest as its words,
// a command substitution among them not known. A word that only looks like a path counts: a
// false crossing errs toward Mixed, the safe way.
//
// Accepted risks: a word with no slash whose expansion cannot be resolved, outside a cd or a
// folder option (a loop over $(ls), rg TODO $DIR), counts for nothing: taking every such
// word for UnknownReach would turn every for loop Mixed. An interpreter's script reaches
// only its slashed words (os.chdir('..'), os.path.join('..', 'x') and Path('..').rglob()
// are not followed), xargs only the words it is given (echo .. | xargs rg reads no ..), and
// zsh's cd old new goes nowhere.
func ShellPaths(command, cwd string) []string {
	return shellPaths(command, cwd, os.LookupEnv)
}

// shellPaths is ShellPaths under env, the hook's environment.
func shellPaths(command, cwd string, env func(string) (string, bool)) []string {
	return shellPathsBy(command, cwd, env, time.Now().Add(maxShellTime))
}

// shellPathsBy is shellPaths, done by deadline.
func shellPathsBy(command, cwd string, env func(string) (string, bool), deadline time.Time) []string {
	return readShell(command, shellReadings(command, windowsShells), cwd, env, deadline)
}

// readShell is what readings of command reach from cwd, done by deadline: the places any
// finds, each once, and UnknownReach when any may reach one it cannot name. Each reading goes
// on from the work those before it did: the scan's bounds are the command's.
func readShell(command string, readings []string, cwd string, env func(string) (string, bool), deadline time.Time) []string {
	var out []string
	done := work{deadline: deadline}
	unknown := false
	for _, src := range readings {
		sc := scanShell(src, cwd, env, done)
		done, unknown = sc.work, unknown || sc.unknown
		for _, p := range sc.out {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	if unknown {
		out = append(out, unknownReach(command))
	}
	return out
}

// windowsShells is whether an agent's shell may be PowerShell or cmd (shellReadings): on
// Windows.
const windowsShells = filepath.Separator == '\\'

// shellReadings are the ways a command is read, where an agent's shell may be PowerShell or
// cmd (windows). A POSIX shell takes a backslash for an escape. On Windows an agent's shell
// may be Git Bash, which does, or PowerShell or cmd, where it separates C:\Users\..., so a
// command holding a backslash is also read with each a slash, and reaches what either
// reading finds: a path only one finds errs toward Mixed.
func shellReadings(command string, windows bool) []string {
	if windows && strings.ContainsRune(command, '\\') {
		return []string{command, windowsReading(command)}
	}
	return []string{command}
}

// windowsReading is command with each backslash a separator, as PowerShell and cmd read it.
func windowsReading(command string) string { return strings.ReplaceAll(command, `\`, "/") }

// scan is one reading of a command: where it is (dir, after its cds; prev before the last;
// pushed, pushd's stack), its assignments (nil for a value not resolved), the script it is
// reading and how deep, what the program whose words it reads names (nil outside one), the
// places it reached in order, what it has looked at, the trees it knows hold no checkout
// (holdsNone), and the work it and the readings before it have done.
type scan struct {
	cwd, dir, prev string
	pushed, none   []string
	env            func(string) (string, bool)
	vars           map[string]*string
	cfg            *expand.Config
	src            string
	depth          int
	reads          *operands
	out            []string
	places         map[string]bool
	examined       map[candidate]string
	work
	unknown bool
}

// work is what a scan has done, counted toward its bounds: the lookups it made, the words it
// expanded and the bytes it parsed and expanded; and the time it must be done by
// (maxShellTime).
type work struct {
	lookups, words, bytes int
	deadline              time.Time
}

// candidate is a word as expanded, from where it was read and in what role, once looked at:
// examined holds the folder it named from there, which a cd's target moves to each time.
type candidate struct {
	word, dir string
	r         role
}

// scanShell reads src, one reading of a command, from cwd, the readings before it having
// done done.
func scanShell(src, cwd string, env func(string) (string, bool), done work) (sc *scan) {
	sc = newScan(cwd, env, done)
	defer func() {
		if recover() != nil {
			sc.unknown = true // the parser or expander tripped on it
		}
	}()
	sc.script(src)
	return sc
}

// newScan is a scan from cwd, with done done, by maxShellTime from now when done has no
// deadline.
func newScan(cwd string, env func(string) (string, bool), done work) *scan {
	if done.deadline.IsZero() {
		done.deadline = time.Now().Add(maxShellTime)
	}
	sc := &scan{cwd: cwd, dir: cwd, env: env, vars: map[string]*string{}, places: map[string]bool{}, examined: map[candidate]string{}, work: done}
	sc.cfg = sc.config()
	return sc
}

// config is a configuration to expand words where the command is.
func (sc *scan) config() *expand.Config {
	return &expand.Config{Env: sc, CmdSubst: sc.substitute, ProcSubst: opaqueProcess, NoUnset: true}
}

// script reads src, a shell script, one level deeper, a statement at a time, once it is
// known to be within the parser's bounds (parsable). One that does not parse (cut
// short, or not bash's: a zsh or fish line) runs up to its error, as a shell does: the
// statements before it are read, and then the rest (rest). One the parser reads otherwise
// than bash does is not known: a comment that starts inside a word ('a'#b in a subshell,
// which bash reads as a#b).
func (sc *scan) script(src string) {
	if sc.depth == maxShellDepth || !sc.charge(len(src)) || !parsable(src) {
		sc.unknown = true
		return
	}
	outer := sc.src
	sc.src, sc.depth = src, sc.depth+1
	defer func() { sc.src, sc.depth = outer, sc.depth-1 }()
	// A statement's here-documents are read only at the end of its line, after it is
	// handed over, so the statements are all parsed before any is read.
	var stmts []*syntax.Stmt
	var err error
	for s, e := range syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).StmtsSeq(strings.NewReader(src)) {
		if err = e; err != nil {
			break
		}
		stmts = append(stmts, s)
	}
	end := 0
	for _, s := range stmts {
		syntax.Walk(s, func(n syntax.Node) bool {
			if c, ok := n.(*syntax.Comment); ok && c.Hash.Offset() > 0 && strings.IndexByte(" \t\n;&|()<>", src[c.Hash.Offset()-1]) < 0 {
				sc.unknown = true
			}
			if n != nil {
				end = max(end, int(n.End().Offset()))
			}
			return true
		})
	}
	sc.stmts(stmts, false)
	if err != nil && !sc.unknown {
		sc.rest(src[min(end, len(src)):])
	}
}

// moves are the commands that move where a command is.
var moves = []string{"cd", "pushd", "popd"}

// rest reads text, the part of a script past what parses, as its distinct words, from where
// the statements before it left the command, each a lookup, split at whitespace and
// operators, quotes dropped, their variables expanded. A cd, pushd or popd among them goes
// somewhere not known, and so does a command substitution's output ($( or a backtick, but
// an arithmetic expansion's $(( ).
func (sc *scan) rest(text string) {
	if strings.Contains(strings.ReplaceAll(text, "$((", ""), "$(") || strings.ContainsRune(text, '`') {
		sc.unknown = true
		return
	}
	parser, seen, unquote := syntax.NewParser(), map[string]bool{}, strings.NewReplacer(`"`, "", "'", "")
	outer := sc.src
	defer func() { sc.src = outer }()
	for w := range strings.FieldsFuncSeq(text, func(r rune) bool { return r <= ' ' || strings.ContainsRune(";&|<>()", r) }) {
		if w = unquote.Replace(w); w == "" || seen[w] {
			continue
		}
		if seen[w] = true; slices.Contains(moves, w) || !sc.spend(1) || !sc.charge(len(w)) {
			sc.unknown = true
			return
		}
		if len(w) > maxShellExpr {
			sc.candidate(w, plain) // too long to parse within the parser's bounds
		} else if word, err := parser.Document(strings.NewReader(w)); err == nil {
			sc.src = w // the word's positions are its own
			sc.arg(word, plain)
		} else {
			sc.candidate(w, plain)
		}
	}
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
