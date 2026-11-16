package hookrun

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
// brace expansion (a word of 4000 took 400 MiB). No command an agent runs comes near them,
// and they keep a pathological one within the hook's budget.
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

// ShellPaths are the places a shell command may reach, for Classify: the folders of the
// existing paths it names (a folder its own place, and a symlink's target's as well), with
// UnknownReach when it may reach one it cannot name. It parses the command as bash does
// (mvdan.cc/sh) and reads each simple command's arguments, a word with no slash included
// when something is there and an option's or assignment's value (--flag=X, -oX, NAME=X)
// as the word, and the slashed words of one holding whitespace (git commit -m "fix a/b")
// that are there; its name, past wrappers (sudo, timeout 60, env A=1, xargs, uv run: the
// wrappers table), only as a path that is there (../other/x); its redirects' targets; its
// assignments' slashed values; a cd's, pushd's and popd's move, which later words resolve
// from as well as the cwd, until a subshell holding it ends; a for loop's body once for
// each of its words; as commands, the script of bash -c and the like and a here-document
// fed to a shell; and the slashed words of python -c, node -e and the like and of any other
// here-document, which reach only what is there. A slashed word naming nothing yet may be
// a file the command creates (touch, > new.md), so it reaches its nearest existing folder;
// a cd to nowhere reaches nothing. A recursive reader (rg, grep -r, find: the readers
// table) of a folder outside any checkout reaches each checkout below it (below).
//
// Words are expanded first (mvdan.cc/sh/v3/expand): braces, variables from the command's
// assignments, its for loops and the hook's environment, $PWD and $(pwd) where the command
// is, the few command substitutions whose output is known (outputs), and globs, walked a
// segment at a time. A word whose expansion cannot be resolved (another command
// substitution's output, an unset variable) is UnknownReach when it may be a path: it is
// slashed, a cd's target, or a folder option's value (-C, --git-dir, --dir...). So is a
// command past any of the scan's bounds or maxGlobSegments. A command that does not parse
// runs up to its error, as bash and zsh do: the statements before it are read as any are,
// and the rest as its words. A word that only looks like a path counts: a false crossing
// errs toward Mixed, the safe way.
//
// Accepted risk: a word with no slash whose expansion cannot be resolved, outside a cd or a
// folder option (a loop over $(ls), rg TODO $DIR), counts for nothing: taking every such
// word for UnknownReach would turn every for loop Mixed.
func ShellPaths(command, cwd string) []string {
	return shellPaths(command, cwd, os.LookupEnv)
}

// shellPaths is ShellPaths under env, the hook's environment.
func shellPaths(command, cwd string, env func(string) (string, bool)) []string {
	var out []string
	var done work
	unknown := false
	for _, src := range shellReadings(command) {
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

// shellReadings are the ways a command is read. A POSIX shell takes a backslash for an
// escape. On Windows an agent's shell may be Git Bash, which does, or PowerShell or cmd,
// where it separates C:\Users\..., so a command holding a backslash is also read with each
// a slash, and reaches what either reading finds: a path only one finds errs toward Mixed.
// The second reading goes on from the work the first did: the scan's bounds are the
// command's.
func shellReadings(command string) []string {
	if filepath.Separator == '\\' && strings.ContainsRune(command, '\\') {
		return []string{command, windowsReading(command)}
	}
	return []string{command}
}

// windowsReading is command with each backslash a separator, as PowerShell and cmd read it.
func windowsReading(command string) string { return strings.ReplaceAll(command, `\`, "/") }

// scan is one reading of a command: where it is (dir, after its cds; prev before the last;
// pushed, pushd's stack), its assignments (nil for a value not resolved), the script it is
// reading and how deep, the places it reached in order, what it has looked at, and the work
// it and the readings before it have done.
type scan struct {
	cwd, dir, prev string
	pushed         []string
	env            func(string) (string, bool)
	vars           map[string]*string
	cfg            *expand.Config
	src            string
	depth          int
	out            []string
	places         map[string]bool
	examined       map[candidate]string
	work
	unknown bool
}

// work is what a scan has done, counted toward its bounds: the lookups it made, the words it
// expanded and the bytes it parsed and expanded.
type work struct{ lookups, words, bytes int }

// candidate is a word as expanded, from where it was read and in what role, once looked at:
// examined holds the folder it named from there, which a cd's target moves to each time.
type candidate struct {
	word, dir string
	r         role
}

// scanShell reads src, one reading of a command, from cwd, the readings before it having
// done done.
func scanShell(src, cwd string, env func(string) (string, bool), done work) (sc *scan) {
	sc = &scan{cwd: cwd, dir: cwd, env: env, vars: map[string]*string{}, places: map[string]bool{}, examined: map[candidate]string{}, work: done}
	sc.cfg = sc.config()
	defer func() {
		if recover() != nil {
			sc.unknown = true // the parser or expander tripped on it
		}
	}()
	sc.script(src)
	return sc
}

// config is a configuration to expand words where the command is.
func (sc *scan) config() *expand.Config {
	return &expand.Config{Env: sc, CmdSubst: sc.substitute, ProcSubst: opaqueProcess, NoUnset: true}
}

// script reads src, a shell script, one level deeper, a statement at a time, once it is
// known to be within the parser's bounds (parsable). One that does not parse (cut
// short, or not bash's: a zsh or fish line) runs up to its error, as a shell does: the
// statements before it are read, and then the rest (rest).
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
	for s, e := range syntax.NewParser(syntax.Variant(syntax.LangBash)).StmtsSeq(strings.NewReader(src)) {
		if err = e; err != nil {
			break
		}
		stmts = append(stmts, s)
	}
	sc.stmts(stmts, false)
	if err != nil && !sc.unknown {
		end := 0
		for _, s := range stmts {
			syntax.Walk(s, func(n syntax.Node) bool {
				if n != nil {
					end = max(end, int(n.End().Offset()))
				}
				return true
			})
		}
		sc.rest(src[min(end, len(src)):])
	}
}

// moves are the commands that move where a command is.
var moves = []string{"cd", "pushd", "popd"}

// rest reads text, the part of a script past what parses, as its distinct words, from where
// the statements before it left the command, each a lookup, split at whitespace and
// operators, quotes dropped, their variables expanded. A cd, pushd or popd among them goes
// somewhere not known.
func (sc *scan) rest(text string) {
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
// maxShellLookups, which makes the reach unknown.
func (sc *scan) spend(n int) bool {
	if sc.unknown || sc.lookups+n > maxShellLookups {
		sc.unknown = true
		return false
	}
	sc.lookups += n
	return true
}

// charge counts n bytes parsed or expanded and reports whether the scan may: not past
// maxShellBytes, which makes the reach unknown.
func (sc *scan) charge(n int) bool {
	if sc.unknown || sc.bytes+n > maxShellBytes {
		sc.unknown = true
		return false
	}
	sc.bytes += n
	return true
}

// candidate looks at t, a word as expanded, in role r (resolve), once from each place in
// each role, then moves where the command is when t is a cd's target that names a folder,
// as often as it is named.
func (sc *scan) candidate(t string, r role) {
	if !r.whole() {
		t = shellValue(t)
	}
	slashed := pathLike(t)
	if t == "" || !slashed && r.needsSlash() {
		return
	}
	c := candidate{t, sc.dir, r}
	dir, seen := sc.examined[c]
	if !seen {
		if !sc.spend(1) {
			return
		}
		dir = sc.resolve(t, r, slashed)
		sc.examined[c] = dir
	}
	if r == target && dir != "" {
		sc.prev, sc.dir = sc.dir, dir
	}
}

// resolve reaches what t, a word in role r, names from where the command is and from the
// cwd, as an earlier cd may have been in a branch not taken or a subshell, and returns the
// folder it names from where the command is, if any. A word that names nothing reaches
// nothing, but one that may be a new file reaches its nearest existing folder.
func (sc *scan) resolve(t string, r role, slashed bool) string {
	p, info := sc.reach(t, sc.dir)
	var back string
	if sc.dir != sc.cwd {
		back, _ = sc.reach(t, sc.cwd)
	}
	if p == "" && back == "" && r.mayCreate(slashed) {
		sc.reachNew(t, sc.dir)
		if sc.dir != sc.cwd {
			sc.reachNew(t, sc.cwd)
		}
	}
	if info != nil && info.IsDir() {
		return p
	}
	return ""
}

// reachEntry adds the place of p, which exists and Lstat described as info: a folder is its
// own, anything else its folder's, and a symlink's target's place as well.
func (sc *scan) reachEntry(p string, info fs.FileInfo) {
	switch {
	case info.IsDir():
		sc.place(p)
	case info.Mode()&fs.ModeSymlink != 0:
		sc.place(filepath.Dir(p))
		if target, err := filepath.EvalSymlinks(p); err == nil {
			if t, err := os.Stat(target); err == nil && t.IsDir() {
				sc.place(target)
			} else {
				sc.place(filepath.Dir(target))
			}
		}
	default:
		sc.place(filepath.Dir(p))
	}
}

// reach adds the place of the path w names from dir when something is there, and returns
// the path with what is there, a symlink followed.
func (sc *scan) reach(w, dir string) (string, os.FileInfo) {
	p, ok := sc.path(w, dir)
	if !ok {
		return "", nil
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", nil
	}
	sc.reachEntry(p, info)
	if info.Mode()&fs.ModeSymlink != 0 {
		if info, err = os.Stat(p); err != nil {
			return p, nil
		}
	}
	return p, info
}

// reachNew adds the nearest existing folder above the path w names from dir, where a file
// the command creates would go; none when the nearest existing path is not a folder.
func (sc *scan) reachNew(w, dir string) {
	p, ok := sc.path(w, dir)
	for dir := filepath.Dir(p); ok; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil {
			if info.IsDir() {
				sc.place(dir)
			}
			return
		}
		ok = filepath.Dir(dir) != dir
	}
}

// path is the path the word w names from dir: ~ is the home directory.
func (sc *scan) path(w, dir string) (string, bool) {
	switch {
	case w == "~" || strings.HasPrefix(w, "~/"):
		home, ok := sc.lookup("HOME")
		return filepath.Join(home, w[1:]), ok
	case filepath.IsAbs(w):
		return filepath.Clean(w), true
	}
	return filepath.Join(dir, w), dir != ""
}
