package hookrun

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// maxShellPaths bounds the candidates ShellPaths examines, keeping a hook that runs on every
// shell call within its budget. Past it the call counts as reaching an unknown organization.
const maxShellPaths = 32

// UnknownReach is a path Classify takes to say the call may have reached a
// checkout no path names: a shell command with more candidates than ShellPaths examines,
// or a path-like word whose expansion it cannot resolve. It is no path (it holds a NUL),
// and the run turns Mixed, as for a checkout of an organization not known.
const UnknownReach = "\x00unknown"

// ShellPaths are the existing paths a shell command may reach, for Classify, with
// UnknownReach when it may reach one it cannot name. Its candidates are each word that is
// absolute, starts with ~/ or holds a /, every cd and pushd target, and each other word
// but a command's name, which the shell looks up on PATH (a here-document's body read as
// data has only its slashed words). A word naming nothing yet may be a file the command
// creates (> new.md, touch, mkdir -p), so a slashed one reaches its nearest existing folder;
// a cd to nowhere reaches nothing. An option's or assignment's value (--flag=X, -oX, NAME=X)
// counts as the word. A relative word resolves against the cwd and against where an earlier
// cd or pushd went.
//
// Words are expanded first: $HOME, $PWD (and $(pwd)), $OLDPWD, an assignment earlier in the
// command, and any other variable the hook's environment sets; a word with a wildcard
// (*, ? or [) to what it matches, each match a candidate. A word that may be a path but
// whose expansion cannot be resolved (a command substitution's output, an unset variable)
// is UnknownReach, as is a candidate past maxShellPaths. It does not parse the shell
// beyond that: a word that only looks like a path counts, and a false crossing errs
// toward Mixed, the safe way.
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
		sc := &shellScan{cwd: cwd, dir: cwd, env: env, vars: map[string]*string{}}
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
	out  []string
	// examined are the candidates looked at, a word from a folder each; past maxShellPaths
	// the scan stops, unknown.
	examined []candidate
	unknown  bool
}

type candidate struct{ word, dir string }

// keep adds p to what the command reaches.
func (sc *shellScan) keep(p string) {
	if !slices.Contains(sc.out, p) {
		sc.out = append(sc.out, p)
	}
}

// examine counts c as examined and reports whether it may be: false once maxShellPaths
// are, which makes the scan unknown, or for one already examined.
func (sc *shellScan) examine(c candidate) bool {
	if slices.Contains(sc.examined, c) {
		return false
	}
	if len(sc.examined) == maxShellPaths {
		sc.unknown = true
		return false
	}
	sc.examined = append(sc.examined, c)
	return true
}

func (sc *shellScan) scan(words []shellWord) {
	cd := false
	for _, w := range words {
		if sc.unknown {
			return // the run turns Mixed whatever else the command names
		}
		text, plain := w.literalText()
		if cd && w.command {
			// A cd with no target goes home.
			cd = false
			if home, ok := sc.lookup("HOME"); ok {
				sc.word(shellWord{parts: []wordPart{{kind: quoted, text: home}}}, expansion{text: home, pattern: escapeGlob(home)}, true)
			}
		}
		target := false
		switch {
		case w.command && plain && (text == "cd" || text == "pushd"):
			cd = true
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

// word looks at one candidate word, x as expanded.
func (sc *shellScan) word(w shellWord, x expansion, target bool) {
	slashed := pathLike(x.text)
	switch {
	case x.text == "":
		return
	case x.unresolved:
		if !w.body && (target || slashed) {
			sc.unknown = true
		}
		return
	case !slashed && !target && (w.command || w.body):
		return // a command's name, or a here-document's data
	case x.glob && sc.glob(x):
		return
	}
	if !sc.examine(candidate{x.text, sc.dir}) {
		return
	}
	p, info := sc.reach(x.text, sc.dir)
	var back string
	if sc.dir != sc.cwd {
		back, _ = sc.reach(x.text, sc.cwd) // after a subshell's cd, the command is back in cwd
	}
	if p == "" && back == "" && !target && slashed {
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

// glob keeps what x's pattern matches, from where the command is and the cwd, each match a
// candidate; false when nothing matches, and the shell passes the word on as written.
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
			if sc.examine(candidate{m, ""}) {
				sc.keep(m)
			}
		}
	}
	return found
}

// reach keeps the path w names from dir when something is there, and returns it.
func (sc *shellScan) reach(w, dir string) (string, os.FileInfo) {
	p, ok := sc.path(w, dir)
	if !ok {
		return "", nil
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", nil
	}
	sc.keep(p)
	return p, info
}

// reachNew keeps the nearest existing folder above the path w names from dir, where a file
// the command creates would go.
func (sc *shellScan) reachNew(w, dir string) {
	if p, ok := sc.path(w, dir); ok {
		if folder, ok := nearestDir(p); ok {
			sc.keep(folder)
		}
	}
}

// expansion is a shell word expanded: its text, the glob pattern it makes (its quoted
// wildcards escaped), whether a wildcard in it is live, and whether some part could not be
// resolved (each such part a NUL in text).
type expansion struct {
	text, pattern string
	glob          bool
	unresolved    bool
}

// unresolvedPart stands in text for a part that could not be resolved.
const unresolvedPart = "\x00"

// expand expands w where the command is now.
func (sc *shellScan) expand(w shellWord) expansion {
	var x expansion
	var text, pattern strings.Builder
	for _, p := range w.parts {
		switch p.kind {
		case literal:
			text.WriteString(p.text)
			pattern.WriteString(p.text)
			x.glob = x.glob || strings.ContainsAny(p.text, "*?[")
		case quoted:
			text.WriteString(p.text)
			pattern.WriteString(escapeGlob(p.text))
		case variable:
			v, ok := sc.lookup(p.text)
			if p.fallback != nil && (!ok || v == "") {
				v, ok = *p.fallback, true
			}
			if !ok {
				x.unresolved = true
				v = unresolvedPart
			}
			text.WriteString(v)
			pattern.WriteString(escapeGlob(v))
		case opaque:
			x.unresolved = true
			text.WriteString(unresolvedPart)
		}
	}
	x.text, x.pattern = text.String(), pattern.String()
	return x
}

// value is x as shellValue reads it: an option's or assignment's value.
func (x expansion) value() expansion {
	v := shellValue(x.text)
	if v == x.text {
		return x
	}
	cut := x.text[:len(x.text)-len(v)]
	if rest, ok := strings.CutPrefix(x.pattern, cut); ok {
		x.pattern = rest
	} else {
		x.pattern, x.glob = v, false // a wildcard escaped before the value: taken as written
	}
	x.text, x.unresolved = v, strings.Contains(v, unresolvedPart)
	return x
}

// lookup is the value of the variable name where the command is now: PWD and OLDPWD as its
// cds left them, an assignment earlier in the command, else the hook's environment, HOME
// falling back to the home directory.
func (sc *shellScan) lookup(name string) (string, bool) {
	switch name {
	case "PWD":
		return sc.dir, sc.dir != ""
	case "OLDPWD":
		if sc.prev != "" {
			return sc.prev, true
		}
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
		if home, err := os.UserHomeDir(); err == nil {
			return home, true
		}
	}
	return "", false
}

// escapeGlob is s with its wildcards escaped for filepath.Match, which on Windows has no
// escape: there they stay live.
func escapeGlob(s string) string {
	if runtime.GOOS == "windows" || !strings.ContainsAny(s, "*?[\\") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("*?[\\", r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
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

// shellValue is the part of the shell word w that may be a path: an option's value after
// its =, or a one-letter option's value written against it (-o/path), an assignment's
// value (NAME=/path), else w itself. An option with no value has none.
func shellValue(w string) string {
	if strings.HasPrefix(w, "-") {
		if _, v, ok := strings.Cut(w, "="); ok {
			return v
		}
		if len(w) > 2 && w[1] != '-' {
			return w[2:]
		}
		return ""
	}
	if name, v, ok := strings.Cut(w, "="); ok && isName(name) {
		return v
	}
	return w
}

// isName reports whether s is a shell variable's name: letters, digits and underscores, not
// starting with a digit.
func isName(s string) bool {
	for i, r := range s {
		letter := r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return s != ""
}

// pathLike reports whether a shell word may name a path: it holds a slash, or on Windows a
// backslash, or is absolute (C:\x). A quoted Windows path keeps its backslashes.
func pathLike(w string) bool {
	return strings.ContainsRune(w, '/') || strings.ContainsRune(w, filepath.Separator) || filepath.IsAbs(w)
}

// path is the path the shell word w names from dir: ~ is the home directory.
func (sc *shellScan) path(w, dir string) (string, bool) {
	switch {
	case w == "~" || strings.HasPrefix(w, "~/"):
		home, ok := sc.lookup("HOME")
		if !ok {
			return "", false
		}
		return filepath.Join(home, w[1:]), true
	case filepath.IsAbs(w):
		return filepath.Clean(w), true
	case dir == "":
		return "", false
	}
	return filepath.Join(dir, w), true
}
