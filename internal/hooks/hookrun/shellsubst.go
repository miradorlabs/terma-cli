package hookrun

import (
	"cmp"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// The command substitutions whose output ShellPaths knows where the command is.

// outputs are the command substitutions whose output ShellPaths knows, by their leading
// words: what each writes where the command is, given the words after those, and whether
// it is known. Any other's is not.
var outputs = map[string]func(sc *scan, args []*syntax.Word) (string, bool){
	"pwd":                           where,
	"pwd -L":                        where,
	"pwd -P":                        where,
	"git rev-parse --show-toplevel": toplevel,
	"go env GOPATH":                 gopath,
	"go env GOMODCACHE":             modcache,
	"mktemp":                        mktemp,
	"dirname":                       ofOperand(dirname),
	"basename":                      ofOperand(func(_ *scan, p string) string { return filepath.Base(p) }),
	"realpath":                      ofOperand((*scan).real),
	"readlink -f":                   ofOperand((*scan).real),
}

// substitute writes the output of the command substitution cs, for expand, when it is one
// of outputs, by the longest of its leading words that is.
func (sc *scan) substitute(w io.Writer, cs *syntax.CmdSubst) error {
	if len(cs.Stmts) != 1 || len(cs.Stmts[0].Redirs) != 0 {
		return errOpaque
	}
	ce, ok := cs.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(ce.Assigns) != 0 {
		return errOpaque
	}
	words := make([]string, len(ce.Args))
	for i, a := range ce.Args {
		words[i] = a.Lit()
	}
	for n := len(words); n > 0; n-- {
		if known, ok := outputs[strings.Join(words[:n], " ")]; ok {
			outer := sc.cfg
			sc.cfg = sc.config() // expand is not reentrant: an operand expands with its own
			out, ok := known(sc, ce.Args[n:])
			sc.cfg = outer
			if !ok {
				return errOpaque
			}
			_, err := io.WriteString(w, out)
			return err
		}
	}
	return errOpaque
}

// where is what pwd writes: where the command is.
func where(sc *scan, args []*syntax.Word) (string, bool) {
	return sc.dir, sc.dir != "" && len(args) == 0
}

// toplevel is what git rev-parse --show-toplevel writes: the root of the checkout the
// command is in.
func toplevel(sc *scan, args []*syntax.Word) (string, bool) {
	root, _, ok := gitx.LocateFS(sc.dir)
	return root, ok && len(args) == 0
}

// gopath is what go env GOPATH writes: $GOPATH, else ~/go.
func gopath(sc *scan, args []*syntax.Word) (string, bool) {
	if v, ok := sc.lookup("GOPATH"); ok && v != "" {
		return v, len(args) == 0
	}
	home, ok := sc.lookup("HOME")
	return filepath.Join(home, "go"), ok && len(args) == 0
}

// modcache is what go env GOMODCACHE writes: $GOMODCACHE, else pkg/mod in GOPATH's first
// folder.
func modcache(sc *scan, args []*syntax.Word) (string, bool) {
	if v, ok := sc.lookup("GOMODCACHE"); ok && v != "" {
		return v, len(args) == 0
	}
	gp, ok := gopath(sc, args)
	return filepath.Join(filepath.SplitList(gp)[0], "pkg", "mod"), ok
}

// mktemp is what mktemp writes: a new file or folder in the temporary folder, or where the
// command is for a template given without -t. One given a folder (-p) or a template with
// a slash is not known.
func mktemp(sc *scan, args []*syntax.Word) (string, bool) {
	name, tmp := "", false
	for _, a := range args {
		switch l := a.Lit(); {
		case l == "-t" || l == "--tmpdir":
			tmp = true
		case l == "" || strings.HasPrefix(l, "-p") || strings.HasPrefix(l, "--tmpdir=") || !strings.HasPrefix(l, "-") && pathLike(l):
			return "", false
		case !strings.HasPrefix(l, "-"):
			name = l
		}
	}
	dir := sc.dir
	if name == "" || tmp {
		dir = os.TempDir()
		if v, ok := sc.lookup("TMPDIR"); ok && v != "" {
			dir = v
		}
	}
	return filepath.Join(dir, cmp.Or(name, "tmp.XXXXXXXXXX")), dir != ""
}

// ofOperand is the output of a command that writes what f makes of its one operand,
// expanded.
func ofOperand(f func(sc *scan, p string) string) func(*scan, []*syntax.Word) (string, bool) {
	return func(sc *scan, args []*syntax.Word) (string, bool) {
		if len(args) > 0 && args[0].Lit() == "--" {
			args = args[1:]
		}
		if len(args) != 1 {
			return "", false
		}
		fields, err := sc.fields(args[0])
		if err != nil || len(fields) != 1 {
			return "", false
		}
		return f(sc, fields[0]), true
	}
}

// dirname is what dirname writes for p: its folder, trailing slashes aside.
func dirname(_ *scan, p string) string {
	if trimmed := strings.TrimRight(p, "/"); trimmed != "" || p == "" {
		return filepath.Dir(trimmed)
	}
	return "/"
}

// real is what realpath and readlink -f write for p: the path it names from where the
// command is, its symlinks followed when it is there.
func (sc *scan) real(p string) string {
	q, ok := sc.path(p, sc.dir)
	if !ok {
		return p
	}
	if r, err := filepath.EvalSymlinks(q); err == nil {
		return r
	}
	return q
}

// opaqueProcess is a process substitution's path, for expand: not known.
func opaqueProcess(*syntax.ProcSubst) (string, error) { return "", errOpaque }
