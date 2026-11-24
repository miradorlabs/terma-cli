package hookrun

import (
	"cmp"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// outputs maps known command-substitution prefixes to static path evaluators.
var outputs = map[string]func(sc *scan, args []*syntax.Word) (string, bool){
	"pwd": where, "pwd -L": where, "pwd -P": whereP, "git rev-parse --show-toplevel": toplevel,
	"go env GOPATH": gopath, "go env GOMODCACHE": modcache, "mktemp": mktemp,
	"dirname":  ofOperand(dirname),
	"basename": ofOperand(func(_ *scan, p string) string { return filepath.Base(p) }),
	"realpath": ofOperand((*scan).real), "readlink -f": ofOperand((*scan).real),
}

func (sc *scan) substitute(w io.Writer, cs *syntax.CmdSubst) error {
	if len(cs.Stmts) != 1 || len(cs.Stmts[0].Redirs) != 0 {
		return errOpaque
	}
	ce, ok := cs.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(ce.Assigns) != 0 {
		return errOpaque
	}
	limit := min(len(ce.Args), 4)
	words := make([]string, limit)
	for i := range limit {
		words[i] = ce.Args[i].Lit()
	}
	for n := limit; n > 0; n-- {
		if known, ok := outputs[strings.Join(words[:n], " ")]; ok {
			outer := sc.cfg
			sc.cfg = sc.config() // expand is not reentrant: an operand expands with its own
			out, ok := known(sc, ce.Args[n:])
			sc.cfg = outer
			if !ok || !sc.charge(len(out)) || !sc.chargeFields(out) {
				return errOpaque
			}
			_, err := io.WriteString(w, out)
			return err
		}
	}
	return errOpaque
}
func where(sc *scan, args []*syntax.Word) (string, bool) {
	return sc.dir, sc.dir != "" && len(args) == 0
}
func whereP(sc *scan, args []*syntax.Word) (string, bool) {
	if sc.dir == "" || len(args) != 0 {
		return "", false
	}
	return sc.real(sc.dir), !sc.unknown
}
func toplevel(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) == 0 {
		return sc.safeLocate(sc.dir)
	}
	return "", false
}
func gopath(sc *scan, args []*syntax.Word) (string, bool) {
	if v, ok := sc.lookup("GOPATH"); ok && v != "" {
		return v, len(args) == 0
	}
	home, ok := sc.lookup("HOME")
	return filepath.Join(home, "go"), ok && len(args) == 0
}
func modcache(sc *scan, args []*syntax.Word) (string, bool) {
	if v, ok := sc.lookup("GOMODCACHE"); ok && v != "" {
		return v, len(args) == 0
	}
	if gp, ok := gopath(sc, args); ok {
		if list := filepath.SplitList(gp); len(list) > 0 && list[0] != "" {
			return filepath.Join(list[0], "pkg", "mod"), true
		}
	}
	return "", false
}
func mktemp(sc *scan, args []*syntax.Word) (string, bool) {
	name, tmp := "", false
	for _, a := range args {
		switch l := a.Lit(); {
		case l == "" || strings.HasPrefix(l, "--tmpdir="):
			return "", false
		case l == "--tmpdir":
			tmp = true
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--"):
			if strings.ContainsRune(l[1:], 'p') {
				return "", false
			}
			tmp = tmp || strings.ContainsRune(l[1:], 't')
		case !strings.HasPrefix(l, "-"):
			if pathLike(l) {
				return "", false
			}
			name = l
		}
	}
	dir := sc.dir
	if name == "" || tmp {
		v, _ := sc.lookup("TMPDIR")
		dir = cmp.Or(v, os.TempDir())
	}
	return filepath.Join(dir, cmp.Or(name, "tmp.XXXXXXXXXX")), dir != ""
}
func ofOperand(f func(sc *scan, p string) string) func(*scan, []*syntax.Word) (string, bool) {
	return func(sc *scan, args []*syntax.Word) (string, bool) {
		if len(args) > 0 && args[0].Lit() == "--" {
			args = args[1:]
		}
		if len(args) == 1 {
			if fields, err := sc.fields(args[0]); err == nil && len(fields) == 1 {
				return f(sc, fields[0]), true
			}
		}
		return "", false
	}
}
func dirname(_ *scan, p string) string {
	if trimmed := strings.TrimRight(p, "/"); trimmed != "" || p == "" {
		return filepath.Dir(trimmed)
	}
	return "/"
}
func (sc *scan) real(p string) string {
	q, ok := sc.path(p, sc.dir)
	if !ok || sc.hasRemoteLink(q) {
		return cmp.Or(q, p)
	}
	r, _ := filepath.EvalSymlinks(q)
	return cmp.Or(r, q)
}
