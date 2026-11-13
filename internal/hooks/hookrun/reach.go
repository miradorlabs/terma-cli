package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// What a tool call reached counts toward a run crossing organizations, not only what it
// edited: a read, a search or a shell command in another organization's checkout carries
// its content into the session's telemetry too. Those calls only classify: Classify places
// them as classifyTouches does, and records and sends nothing else.

// Classify is how a hook sees a tool call that read, searched or ran a shell command at
// paths, each a file or a directory, absolute or relative to Cwd: on a migrated machine it
// classifies the checkouts they reach with the session's own location r and the run's
// earlier locations (classifyTouches), and the run ledger records them. It writes no
// manifest and spools nothing. A nil r is located here, with no session store and no
// parked records handed on: a hook that only classifies, before its tool call runs, or a
// location Route drops, which Repo refused, whose run is still classified and recorded so
// that a later hook of the run in a collected location sees the crossing. Elsewhere it
// does nothing.
func (e Env) Classify(ctx context.Context, r *Repo, sess session.Session, paths []string, extra map[string]any) {
	paths = slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return p == "" })
	if e.Routing == nil || !e.Routing.Usable() || len(paths) == 0 || !session.ValidID(sess.ID) {
		return
	}
	if r == nil {
		var err error
		if r, err = e.routeHere(ctx); err != nil {
			return
		}
	}
	e.classifyTouches(r, sess, extra, e.reached(r, paths))
}

// reached are the checkouts other than r that paths reach, each once: a directory's own,
// a file's (or a path's that is not there) its folder's. A path in no checkout reaches none.
func (e Env) reached(r *Repo, paths []string) []touched {
	var out []touched
	dirs := map[string]bool{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(e.Cwd, p)
		}
		dir := filepath.Clean(p)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			dir = filepath.Dir(dir)
		}
		if dirs[dir] {
			continue
		}
		dirs[dir] = true
		repo := e.checkoutOf(r, dir)
		if repo == nil || repo == r || slices.ContainsFunc(out, func(t touched) bool { return samePath(t.repo.Root, repo.Root) }) {
			continue
		}
		out = append(out, touched{repo: repo})
	}
	return out
}

// maxShellPaths bounds the candidate paths ShellPaths examines in one command, so a hook
// that runs on every shell call keeps to its budget whatever the command.
const maxShellPaths = 32

// ShellPaths are the paths a shell command may reach, for Classify: each word that is
// absolute, starts with ~/ or holds a /, and the target of every cd and pushd, that exists.
// A word that names nothing there yet may be a file the command creates (> new.md, touch,
// mkdir -p): it reaches its nearest folder that is there, whose checkout it would be in;
// a cd or pushd to nowhere goes nowhere. A word that is an option's or an assignment's
// value (--flag=X, -oX, NAME=X) is that value. A relative word resolves against the cwd, and against where an earlier cd or
// pushd in the command went, since it may run there. Words are split as shellWords splits
// them. It does not parse the shell: a word that only looks like a path counts too, and a
// run it wrongly finds crossing turns Mixed and stops sending, the safe direction.
//
// Accepted risk: only a command's first maxShellPaths candidates are examined, so a
// crossing only a later one makes is missed. Examining every one would put no bound on a
// hook that runs on every shell call.
func ShellPaths(command, cwd string) []string {
	var out []string
	keep := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	// reach keeps the path w names from dir when something is there, and returns it.
	reach := func(w, dir string) (string, os.FileInfo) {
		p, ok := shellPath(w, dir)
		if !ok {
			return "", nil
		}
		info, err := os.Stat(p)
		if err != nil {
			return "", nil
		}
		keep(p)
		return p, info
	}
	// reachNew keeps the nearest folder that is there above the path w names from dir: the
	// folder a file the command creates would be in.
	reachNew := func(w, dir string) {
		if p, ok := shellPath(w, dir); ok {
			if folder, ok := nearestDir(p); ok {
				keep(folder)
			}
		}
	}
	type candidate struct{ word, dir string }
	var examined []candidate
	dir, cd := cwd, false
	for _, w := range shellWords(command) {
		target := false
		switch {
		case w == "cd" || w == "pushd":
			cd = true
			continue
		case cd && strings.HasPrefix(w, "-"):
			continue // one of cd's options
		case cd:
			target, cd = true, false
		default:
			w = shellValue(w)
		}
		c := candidate{w, dir}
		if w == "" || !target && !strings.Contains(w, "/") || slices.Contains(examined, c) {
			continue
		}
		if len(examined) == maxShellPaths {
			break
		}
		examined = append(examined, c)
		p, info := reach(w, dir)
		var back string
		if dir != cwd {
			back, _ = reach(w, cwd) // after a subshell's cd, the command is back in cwd
		}
		if p == "" && back == "" && !target {
			// There in neither place: a file the command may create, in either.
			reachNew(w, dir)
			if dir != cwd {
				reachNew(w, cwd)
			}
		}
		if target && info != nil && info.IsDir() {
			dir = p // the words after a cd that went somewhere may run there
		}
	}
	return out
}

// nearestDir is the nearest folder above p, a path not there, that is there; false when
// what is there first is not a folder.
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

// shellPath is the path the shell word w names, from cwd: ~ is the home directory.
func shellPath(w, cwd string) (string, bool) {
	switch {
	case w == "~" || strings.HasPrefix(w, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return filepath.Join(home, w[1:]), true
	case filepath.IsAbs(w):
		return filepath.Clean(w), true
	case cwd == "":
		return "", false
	}
	return filepath.Join(cwd, w), true
}

// shellWords splits command into words at whitespace, the operators ;, &, |, < and > (so
// &&, ||, >> and 2>&1 too) and the parentheses and backticks around a subshell or a command
// substitution, outside quotes, dropping the quotes and a backslash's escape. A command
// substitution inside double quotes, "$(...)" or "`...`", is split as a command of its own.
func shellWords(command string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	// open are the subshells and substitutions entered and not yet left, each with what
	// closes it and the quote it was entered from, which its end returns to.
	type nested struct{ close, quote rune }
	var open []nested
	end := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	enter := func(closer rune) {
		end()
		open = append(open, nested{close: closer, quote: quote})
		quote = 0
	}
	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		substitution := c == '$' && i+1 < len(rs) && rs[i+1] == '('
		switch {
		case escaped:
			// Inside double quotes a backslash escapes only what is special there.
			if quote == '"' && !strings.ContainsRune("\"\\$`", c) {
				cur.WriteRune('\\')
			}
			cur.WriteRune(c)
			escaped = false
		case quote == '\'':
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"' && substitution:
			i++ // past the (
			enter(')')
		case quote == '"' && c == '`':
			enter('`')
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\':
			escaped, inWord = true, true
		case len(open) > 0 && c == open[len(open)-1].close:
			end()
			quote, open = open[len(open)-1].quote, open[:len(open)-1]
			inWord = quote != 0
		case c == '(':
			enter(')')
		case c == '`':
			enter('`')
		case substitution:
			end() // the ( that follows enters it
		case unicode.IsSpace(c) || strings.ContainsRune(";&|<>)", c):
			end()
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	end()
	return words
}
