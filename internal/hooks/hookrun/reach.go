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
// manifest and spools nothing. A nil r is a location Route drops, which Repo refused: it
// is located here, and its run is still classified and recorded, so that a later hook of
// the run in a collected location sees the crossing. Elsewhere it does nothing.
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
// absolute, starts with ~/ or holds a /, and the target of every cd, resolved against cwd,
// that exists. Words are split at whitespace and at ;, &&, ||, |, >, >> and <, outside
// quotes, with the quotes removed. It does not parse the shell: a word that only looks
// like a path counts too, and a run it wrongly finds crossing turns Mixed and stops
// sending, the safe direction.
//
// Accepted risk: only a command's first maxShellPaths candidates are examined, so a
// crossing only a later one makes is missed. Examining every one would put no bound on a
// hook that runs on every shell call.
func ShellPaths(command, cwd string) []string {
	var candidates []string
	words := shellWords(command)
	for i, w := range words {
		cdTarget := i > 0 && words[i-1] == "cd" && !strings.HasPrefix(w, "-")
		if w == "" || !cdTarget && !strings.Contains(w, "/") {
			continue
		}
		if !slices.Contains(candidates, w) {
			candidates = append(candidates, w)
		}
	}
	candidates = candidates[:min(len(candidates), maxShellPaths)]
	var out []string
	for _, w := range candidates {
		p, ok := shellPath(w, cwd)
		if !ok {
			continue
		}
		if _, err := os.Stat(p); err == nil && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
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

// shellWords splits command into words at whitespace and the operators ;, &, |, < and >
// (so &&, ||, >> and 2>&1 too) outside quotes, dropping the quotes and a backslash's escape.
func shellWords(command string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	end := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for _, c := range command {
		switch {
		case escaped:
			// Inside double quotes a backslash escapes only what is special there.
			if quote == '"' && !strings.ContainsRune("\"\\$`", c) {
				cur.WriteRune('\\')
			}
			cur.WriteRune(c)
			escaped = false
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				escaped = true
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\':
			escaped, inWord = true, true
		case unicode.IsSpace(c) || strings.ContainsRune(";&|<>", c):
			end()
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	end()
	return words
}
