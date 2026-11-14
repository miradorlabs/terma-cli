package hookrun

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// How ShellPaths reads a word: expanded where the command is, and what of it may be a path.

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
	case topLevel:
		if sc.dir == "" {
			return "", false
		}
		root, _, ok := gitx.LocateFS(sc.dir)
		return root, ok
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
