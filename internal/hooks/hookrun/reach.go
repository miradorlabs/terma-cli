package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// A read, a search or a shell command in another organization's checkout carries its
// content into the session's telemetry, so what a call reached counts toward a crossing,
// not only what it edited. Such calls only classify: they record and send nothing else.

// Classify classifies, on a migrated machine, the checkouts that paths (files or folders,
// absolute or relative to Cwd) reach, with the session's location r and the run's earlier
// locations (classifyTouches). It writes no manifest and spools nothing. A nil r is located
// here, with no store and nothing unparked: for a hook before its tool call runs, or in a
// location Route drops, so a later hook still sees the crossing. It returns r, or the
// checkout it located, for RepoAt; nil for none.
func (e Env) Classify(ctx context.Context, r *Repo, sess session.Session, paths []string, extra map[string]any) *Repo {
	paths = slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return p == "" })
	if e.Routing == nil || !e.Routing.Usable() || len(paths) == 0 || !session.ValidID(sess.ID) {
		return r
	}
	if r == nil {
		var err error
		if r, err = e.routeHere(ctx); err != nil {
			e.classifyUnlocated(sess, extra, e.reached(nil, paths))
			return nil
		}
	}
	e.classifyTouches(r, sess, extra, e.reached(r, paths))
	return r
}

// classifyUnlocated is classifyTouches for a hook whose own checkout cannot be located (its
// cwd deleted, say): what the call reached is classified against where the run has been.
// It writes a placement only if the run turns Mixed.
//
// Accepted risk: the hook's own location is not classified. Marking the run Mixed for it
// would turn every session whose cwd was deleted Mixed. The run's earlier hooks recorded
// where it ran, and the next hook that can locate its checkout classifies it.
func (e Env) classifyUnlocated(sess session.Session, extra map[string]any, reached []touched) {
	var locs []routing.Location
	for _, t := range reached {
		if t.repo.dest != nil {
			locs = append(locs, t.repo.dest.loc)
		}
	}
	if len(locs) == 0 {
		return
	}
	agent, _ := extra[semconv.TermaAgentIDKey].(string)
	e.judge(sess.ID, agent, claim.Claim{Tool: sess.Tool, PIDs: claimPIDs()}, locs, false)
}

// reached are the checkouts other than r (which may be nil) that paths reach, each once: a
// folder's own, else its parent's. A path in no checkout reaches none.
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

// maxShellPaths bounds the candidates ShellPaths examines, keeping a hook that runs on every
// shell call within its budget.
const maxShellPaths = 32

// ShellPaths are the existing paths a shell command may reach, for Classify: each word that
// is absolute, starts with ~/ or holds a /, and every cd and pushd target. A word naming
// nothing yet may be a file the command creates (> new.md, touch, mkdir -p), so it reaches
// its nearest existing folder; a cd to nowhere reaches nothing. An option's or assignment's
// value (--flag=X, -oX, NAME=X) counts as the word. A relative word resolves against the
// cwd and against where an earlier cd or pushd went. It does not parse the shell: a word
// that only looks like a path counts, and a false crossing errs toward Mixed, the safe way.
//
// Accepted risk: only the first maxShellPaths candidates are examined, so a crossing only a
// later one makes is missed. An unbounded scan would put no bound on a hook that runs on
// every shell call.
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
	// reachNew keeps the nearest existing folder above the path w names from dir, where a
	// file the command creates would go.
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

// shellWords splits command into words outside quotes: at whitespace, at ;, &, |, < and >
// (so &&, ||, >>, 2>&1), and around subshells and command substitutions, those inside
// double quotes too. Quotes and escaping backslashes are dropped.
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
