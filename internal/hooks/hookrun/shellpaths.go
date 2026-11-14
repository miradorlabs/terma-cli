package hookrun

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxShellPlaces bounds the places a shell command may reach: the folders ShellPaths
// returns, each once however many of the command's words name it or something in it. Past
// it the command may reach a checkout the scan does not name. Each place costs Classify a
// checkout lookup, some 20µs, so the bound keeps a hook that runs on every shell call well
// within its budget, while no command an agent runs comes near it.
const maxShellPlaces = 256

// maxShellLookups bounds the scan's own work: a lookup for each distinct candidate word and
// each glob match, about 2µs each. Past it the reach is not known. It keeps a pathological
// command (a megabyte of words) within the hook's budget.
const maxShellLookups = 4096

// UnknownReach is a path Classify takes to say the call may have reached a checkout no path
// names: a shell command reaching more places than ShellPaths returns, or a path-like word
// whose expansion it cannot resolve. It is no path (it holds a NUL), and the run turns
// Mixed, as for a checkout of an organization not known.
const UnknownReach = "\x00unknown"

// ShellPaths are the places a shell command may reach, for Classify: the folders of the
// existing paths it names (a folder its own place, and a symlink's target's as well), with
// UnknownReach when it may reach one it cannot name. Its candidates are each word that is
// absolute, starts with ~/ or holds a /, every cd and pushd target, and each other word
// but the command's name, the program to run, which is never a candidate, even with a
// slash or a part not resolved ($(go env GOPATH)/bin/lint): a here-document's body read as
// data has only its slashed words. A word naming nothing yet may be a file the command
// creates (> new.md, touch, mkdir -p), so a slashed one reaches its nearest existing folder,
// except in a here-document's body, which creates nothing; a cd to nowhere reaches nothing.
// An option's or assignment's value (--flag=X, -oX, NAME=X) counts as the word. A relative
// word resolves against the cwd and against where an earlier cd or pushd went.
//
// Words are expanded first: $HOME, $PWD (and $(pwd)), $OLDPWD, the checkout's root for
// $(git rev-parse --show-toplevel), an assignment earlier in the command, a for loop's
// variable (each of its words in turn), and any other variable the hook's environment sets; a word with a wildcard
// (*, ? or [) to what it matches, each match reaching its folder. A word that may be a path
// but whose expansion cannot be resolved (a command substitution's output, an unset
// variable) is UnknownReach, as is a command past maxShellPlaces or maxShellLookups. It
// does not parse the shell beyond that: a word that only looks like a path counts, and a
// false crossing errs toward Mixed, the safe way.
//
// Accepted risk: a word with no slash whose expansion cannot be resolved (a loop's
// variable, rg TODO $DIR) counts for nothing: taking every such word for UnknownReach
// would turn every for loop Mixed.
func ShellPaths(command, cwd string) []string {
	return shellPaths(command, cwd, os.LookupEnv)
}

// shellPaths is ShellPaths under env, the hook's environment.
func shellPaths(command, cwd string, env func(string) (string, bool)) []string {
	var out []string
	unknown := false
	for _, escapes := range shellReadings() {
		sc := &shellScan{cwd: cwd, dir: cwd, env: env, vars: map[string]*string{}, places: map[string]bool{}, examined: map[candidate]bool{}}
		sc.scan(shellWords(command, escapes))
		unknown = unknown || sc.unknown
		for _, p := range sc.out {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	if unknown {
		out = append(out, UnknownReach)
	}
	return out
}

// shellReadings are the ways a command is read, each whether a backslash escapes. A POSIX
// shell escapes with it. On Windows an agent's shell may be Git Bash, which escapes, or
// PowerShell or cmd, where it separates C:\Users\..., so a command is
// read both ways and reaches what either reading finds: a path only one finds errs toward
// Mixed, the safe way.
func shellReadings() []bool {
	if filepath.Separator == '\\' {
		return []bool{true, false}
	}
	return []bool{true}
}

// shellScan is one ShellPaths: where the command is, what it has assigned, and what it
// found so far.
type shellScan struct {
	cwd string
	// dir is where the command is now, after its cds; prev where it was before the last.
	dir, prev string
	env       func(string) (string, bool)
	// vars are the command's assignments so far, nil for a value not resolved.
	vars map[string]*string
	// out are the places reached, in the order found; places holds them.
	out    []string
	places map[string]bool
	// examined are the candidates looked at, a word from a folder each, and lookups the
	// lookups made; past maxShellLookups the scan stops, unknown.
	examined map[candidate]bool
	lookups  int
	unknown  bool
}

// candidate is a word as expanded, whether its wildcards are live, and where it was read.
type candidate struct {
	word string
	glob bool
	dir  string
}

// place adds the folder p to what the command reaches; past maxShellPlaces the scan stops,
// unknown.
func (sc *shellScan) place(p string) {
	switch {
	case sc.places[p]:
	case len(sc.places) == maxShellPlaces:
		sc.unknown = true
	default:
		sc.places[p] = true
		sc.out = append(sc.out, p)
	}
}

// reachEntry adds the place of p, which exists and Lstat described as info: a folder is its
// own, anything else its folder's, and a symlink's target's place as well.
func (sc *shellScan) reachEntry(p string, info fs.FileInfo) {
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

// spend counts a lookup and reports whether the scan may make it: false past
// maxShellLookups, which makes the reach unknown.
func (sc *shellScan) spend() bool {
	if sc.lookups == maxShellLookups {
		sc.unknown = true
		return false
	}
	sc.lookups++
	return true
}

// scan reads words in order. A cd or pushd waits for its target, the next word of its own
// depth: a word inside a substitution comes before the word holding it, which is the
// target, so it is read as any other word and leaves the cd waiting.
func (sc *shellScan) scan(words []shellWord) {
	cd, cdDepth := false, 0
	for i := 0; i < len(words); i++ {
		if sc.unknown {
			return // the run turns Mixed whatever else the command names
		}
		w := words[i]
		text, plain := w.literalText()
		if cd && (w.depth < cdDepth || w.depth == cdDepth && w.command) {
			// A cd with no target goes home.
			cd = false
			if home, ok := sc.lookup("HOME"); ok {
				sc.word(shellWord{parts: []wordPart{{kind: quoted, text: home}}}, expansion{text: home, pattern: escapeGlob(home)}, true)
			}
		}
		if w.command && plain && text == "for" {
			if done, ok := sc.loop(words, i); ok {
				i = done
				continue
			}
		}
		target := false
		switch {
		case cd && w.depth > cdDepth:
			// Inside the substitution that makes the cd's target.
		case w.command && plain && (text == "cd" || text == "pushd"):
			cd, cdDepth = true, w.depth
			continue
		case cd && plain && text == "-":
			// Back where it was, which it already reached.
			cd = false
			if sc.prev != "" {
				sc.prev, sc.dir = sc.dir, sc.prev
			}
			continue
		case cd && plain && strings.HasPrefix(text, "-"):
			continue // one of cd's options
		case cd:
			target, cd = true, false
		}
		x := sc.expand(w)
		if !target {
			if isAssignment(w) {
				name, value, _ := strings.Cut(x.text, "=")
				if x.unresolved {
					sc.vars[name] = nil
				} else {
					sc.vars[name] = &value
				}
			}
			x = x.value()
		}
		sc.word(w, x, target)
	}
}

// loop reads the for loop at words[i], for NAME in WORDS; do BODY; done: its WORDS are
// candidates as any command's, and BODY is read once for each, NAME bound to it in turn,
// each pass a lookup (with no in, NAME is not known). It returns where the loop's done is;
// false for words that are no such loop, read as written.
func (sc *shellScan) loop(words []shellWord, i int) (int, bool) {
	keyword := func(j int, kw string) bool {
		text, plain := words[j].literalText()
		return plain && text == kw && words[j].command && !words[j].body && words[j].depth == words[i].depth
	}
	if i+1 == len(words) {
		return 0, false
	}
	name, plain := words[i+1].literalText()
	if !plain || !isName(name) {
		return 0, false
	}
	do, done, nested := -1, -1, 0
	for j := i + 2; j < len(words) && done < 0; j++ {
		switch {
		case !keyword(j, "do") && !keyword(j, "done"):
		case do < 0:
			if keyword(j, "do") {
				do = j
			}
		case keyword(j, "do"):
			nested++
		case nested > 0:
			nested--
		default:
			done = j
		}
	}
	if done < 0 {
		return 0, false
	}
	list, body := words[i+2:do], words[do+1:done]
	in := false
	if len(list) > 0 {
		text, plain := list[0].literalText()
		if in = plain && text == "in"; in {
			list = list[1:]
		}
	}
	sc.scan(list)
	pass := func(v *string) {
		if sc.vars[name] = v; !sc.unknown && sc.spend() {
			sc.scan(body)
		}
	}
	if !in {
		pass(nil)
	}
	for _, w := range list {
		if x := sc.expand(w); x.unresolved {
			pass(nil)
		} else {
			pass(&x.text)
		}
	}
	return done, true
}

// word looks at one candidate word, x as expanded.
func (sc *shellScan) word(w shellWord, x expansion, target bool) {
	slashed := pathLike(x.text)
	switch {
	case w.command && !w.body && !target && !isAssignment(w):
		return // the program to run, whatever it expands to: no place the command reaches
	case x.text == "":
		return
	case x.unresolved:
		if !w.body && (target || slashed) {
			sc.unknown = true
		}
		return
	case !slashed && !target && (w.command || w.body):
		return // an assignment's value before the command, or a here-document's data
	}
	c := candidate{x.text, x.glob, sc.dir}
	if sc.examined[c] {
		return
	}
	sc.examined[c] = true
	if x.glob && sc.glob(x) || !sc.spend() {
		return
	}
	p, info := sc.reach(x.text, sc.dir)
	var back string
	if sc.dir != sc.cwd {
		back, _ = sc.reach(x.text, sc.cwd) // after a subshell's cd, the command is back in cwd
	}
	if p == "" && back == "" && !target && slashed && !w.body {
		// There in neither place: a file the command may create, in either.
		sc.reachNew(x.text, sc.dir)
		if sc.dir != sc.cwd {
			sc.reachNew(x.text, sc.cwd)
		}
	}
	if target && info != nil && info.IsDir() {
		sc.prev, sc.dir = sc.dir, p // the words after a cd that went somewhere may run there
	}
}

// glob reaches the places x's pattern matches, from where the command is and the cwd: a
// match's folder, or a matched folder itself, each once however many matches it holds.
// False when nothing matches, and the shell passes the word on as written.
func (sc *shellScan) glob(x expansion) bool {
	dirs := []string{sc.dir}
	if sc.dir != sc.cwd {
		dirs = append(dirs, sc.cwd)
	}
	found := false
	for _, dir := range dirs {
		pattern, ok := sc.path(x.pattern, dir)
		if !ok {
			continue
		}
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			found = true
			if !sc.spend() {
				return true
			}
			if info, err := os.Lstat(m); err == nil {
				sc.reachEntry(m, info)
			}
		}
	}
	return found
}

// reach adds the place of the path w names from dir when something is there, and returns
// the path with what is there, a symlink followed.
func (sc *shellScan) reach(w, dir string) (string, os.FileInfo) {
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
// the command creates would go.
func (sc *shellScan) reachNew(w, dir string) {
	if p, ok := sc.path(w, dir); ok {
		if folder, ok := nearestDir(p); ok {
			sc.place(folder)
		}
	}
}

// nearestDir is the nearest existing folder above p, which does not exist; false when the
// nearest existing path is not a folder.
func nearestDir(p string) (string, bool) {
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil {
			return dir, info.IsDir()
		}
		if filepath.Dir(dir) == dir {
			return "", false
		}
	}
}
