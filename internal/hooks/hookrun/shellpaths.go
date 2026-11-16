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
// candidate, each entry a glob reads, each word braces make, and each pass of a for loop,
// charged its body's statements. maxShellWords bounds the words it expands, about 1µs each,
// those that cost no lookup included; maxShellBytes the command it parses, a megabyte in
// some 8ms; maxShellDepth the scripts it reads inside each other (bash -c). No command an
// agent runs comes near them, and they keep a pathological one within the hook's budget.
const (
	maxShellPlaces  = 256
	maxShellLookups = 4096
	maxShellWords   = 4 * maxShellLookups
	maxShellBytes   = 1 << 20
	maxShellDepth   = 4
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

// firstWords are command's first four words, at most 60 characters, an assignment's or an
// option's value left out (NAME=…, --password=…, -p…): enough for doctor to name the
// command, and no more of it on disk.
func firstWords(command string) string {
	words := strings.Fields(command)
	more := len(words) > 4
	if more {
		words = words[:4]
	}
	for i, w := range words {
		if name, _, ok := strings.Cut(w, "="); ok && name != "" {
			words[i] = name + "=…"
		} else if len(w) > 2 && w[0] == '-' && w[1] != '-' {
			words[i] = w[:2] + "…"
		}
	}
	s := strings.Join(words, " ")
	if r := []rune(s); len(r) > 60 {
		s, more = string(r[:60]), true
	}
	if more {
		s += " …"
	}
	return s
}

// ShellPaths are the places a shell command may reach, for Classify: the folders of the
// existing paths it names (a folder its own place, and a symlink's target's as well), with
// UnknownReach when it may reach one it cannot name. It parses the command as bash does
// (mvdan.cc/sh) and reads each simple command's arguments, a word with no slash included
// when something is there and an option's or assignment's value (--flag=X, -oX, NAME=X)
// as the word; its name, past nohup, exec, time and the like, only as a path that is there
// (../other/x); its redirects' targets; its assignments' slashed values; a cd's, pushd's
// and popd's move, which later words resolve from as well as the cwd, until a subshell
// holding it ends; a for loop's body once for each of its words; as commands, the script
// of bash -c and the like and a here-document fed to a shell; and the slashed words of
// python -c, node -e and the like and of any other here-document, which reach only what
// is there. A slashed word naming nothing yet may be a file the command creates (touch,
// > new.md), so it reaches its nearest existing folder; a cd to nowhere reaches nothing.
//
// Words are expanded first (mvdan.cc/sh/v3/expand): braces, variables from the command's
// assignments, its for loops and the hook's environment, $PWD and $(pwd) where the command
// is, $(git rev-parse --show-toplevel) as that checkout's root, and globs, walked a
// segment at a time. A word whose expansion cannot be resolved (another command
// substitution's output, an unset variable) is UnknownReach when it may be a path: it is
// slashed, a cd's target, or a folder option's value (-C, --git-dir, --dir...). So is a
// command past any of the scan's bounds or maxGlobSegments. A command that does not parse
// is read as its words, split at whitespace and operators. A word that only looks like a
// path counts: a false crossing errs toward Mixed, the safe way.
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
	unknown := false
	for _, src := range shellReadings(command) {
		sc := scanShell(src, cwd, env)
		unknown = unknown || sc.unknown
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
// where it separates C:\Users\..., so a command is also read with each backslash a slash,
// and reaches what either reading finds: a path only one finds errs toward Mixed.
func shellReadings(command string) []string {
	if filepath.Separator == '\\' {
		return []string{command, windowsReading(command)}
	}
	return []string{command}
}

// windowsReading is command with each backslash a separator, as PowerShell and cmd read it.
func windowsReading(command string) string { return strings.ReplaceAll(command, `\`, "/") }

// scan is one reading of a command: where it is (dir, after its cds; prev before the last;
// pushed, pushd's stack), its assignments (nil for a value not resolved), the script it is
// reading and how deep, the places it reached in order, and the work it has done.
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
	examined       map[candidate]bool
	lookups, words int
	unknown        bool
}

// candidate is a word as expanded, from where it was read and as what (a cd's target, a
// glob), once looked at.
type candidate struct {
	word, dir string
	kind      role
}

// scanShell reads src, one reading of a command, from cwd.
func scanShell(src, cwd string, env func(string) (string, bool)) (sc *scan) {
	sc = &scan{cwd: cwd, dir: cwd, env: env, vars: map[string]*string{}, places: map[string]bool{}, examined: map[candidate]bool{}}
	sc.cfg = &expand.Config{Env: sc, CmdSubst: sc.substitute, ProcSubst: opaqueProcess, NoUnset: true}
	defer func() {
		if recover() != nil {
			sc.unknown = true // the parser or expander tripped on it
		}
	}()
	sc.script(src)
	return sc
}

// script reads src, a shell script, one level deeper, once it is known to be within the
// parser's bounds (parsable). One that does not parse (cut short, or not a POSIX shell's)
// is read as its distinct words, each a lookup, split at whitespace and operators, quotes
// dropped, their variables expanded.
func (sc *scan) script(src string) {
	if len(src) > maxShellBytes || sc.depth == maxShellDepth || !parsable(src) {
		sc.unknown = true
		return
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err == nil {
		outer := sc.src
		sc.src, sc.depth = src, sc.depth+1
		sc.stmts(file.Stmts, false)
		sc.src, sc.depth = outer, sc.depth-1
		return
	}
	parser, seen, unquote := syntax.NewParser(), map[string]bool{}, strings.NewReplacer(`"`, "", "'", "")
	for w := range strings.FieldsFuncSeq(src, func(r rune) bool { return r <= ' ' || strings.ContainsRune(";&|<>()", r) }) {
		if w = unquote.Replace(w); w == "" || seen[w] {
			continue
		}
		if seen[w] = true; !sc.spend(1) {
			return
		}
		if len(w) > maxShellExpr {
			sc.candidate(w, 0) // too long to parse within the parser's bounds
		} else if word, err := parser.Document(strings.NewReader(w)); err == nil {
			sc.arg(word, 0)
		} else {
			sc.candidate(w, 0)
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

// candidate looks at t, a word as expanded, in role r, from where the command is and from
// the cwd, as an earlier cd may have been in a branch not taken or a subshell. A word that
// names nothing reaches nothing, but a slashed or created one may be a new file, which
// reaches its nearest existing folder.
func (sc *scan) candidate(t string, r role) {
	if r&(command|target) == 0 {
		t = shellValue(t)
	}
	slashed := pathLike(t)
	if t == "" || !slashed && r&(command|data|slashedOnly) != 0 {
		return
	}
	c := candidate{t, sc.dir, r & target}
	if sc.examined[c] || !sc.spend(1) {
		return
	}
	sc.examined[c] = true
	p, info := sc.reach(t, sc.dir)
	var back string
	if sc.dir != sc.cwd {
		back, _ = sc.reach(t, sc.cwd)
	}
	if p == "" && back == "" && (slashed || r&creates != 0) && r&(command|target|data) == 0 {
		sc.reachNew(t, sc.dir)
		if sc.dir != sc.cwd {
			sc.reachNew(t, sc.cwd)
		}
	}
	if r&target != 0 && info != nil && info.IsDir() {
		sc.prev, sc.dir = sc.dir, p
	}
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
