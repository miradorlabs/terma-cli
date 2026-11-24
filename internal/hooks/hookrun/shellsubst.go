package hookrun

import (
	"cmp"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// outputs maps known command-substitution prefixes to static path evaluators.
var outputs = map[string]func(sc *scan, args []*syntax.Word) (string, bool){
	"pwd": where, "pwd -L": where, "pwd -P": whereP, "git rev-parse --show-toplevel": toplevel,
	"go env GOPATH": gopath, "go env GOMODCACHE": modcache, "mktemp": mktemp,
	"dirname":  ofOperand(dirname),
	"basename": ofOperand(func(_ *scan, p string) (string, bool) { return filepath.Base(p), true }),
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
	words := make([]string, min(len(ce.Args), 4))
	for i := range words {
		words[i] = ce.Args[i].Lit()
	}
	for n := len(words); n > 0; n-- {
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
	r, ok := sc.real(sc.dir)
	return r, ok && !sc.unknown
}
func toplevel(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) == 0 {
		return sc.safeLocate(sc.dir)
	}
	return "", false
}
func (sc *scan) hasGoEnv() bool {
	if sc.isUnresolvedVar("GOENV") {
		return true
	}
	p, _ := sc.lookup("GOENV")
	if p == "off" {
		return false
	}
	if p == "" {
		var dir string
		if runtime.GOOS == "windows" {
			dir, _ = sc.lookup("AppData")
		} else if xdg, _ := sc.lookup("XDG_CONFIG_HOME"); runtime.GOOS != "darwin" && xdg != "" {
			dir = xdg
		} else if home, ok := sc.lookup("HOME"); ok {
			dir = filepath.Join(home, ".config")
			if runtime.GOOS == "darwin" {
				dir = filepath.Join(home, "Library", "Application Support")
			}
		}
		if dir == "" {
			return false
		}
		p = filepath.Join(dir, "go", "env")
	}
	if !filepath.IsAbs(p) || sc.hasRemoteLink(p) || !sc.spendStat(1) {
		return true
	}
	_, err := os.Lstat(p)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
func gopath(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOPATH") {
		return "", false
	}
	if v, ok := sc.lookup("GOPATH"); ok && v != "" {
		return v, true
	}
	if home, ok := sc.lookup("HOME"); !sc.hasGoEnv() && ok {
		return filepath.Join(home, "go"), true
	}
	return "", false
}
func modcache(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOMODCACHE") {
		return "", false
	}
	if v, ok := sc.lookup("GOMODCACHE"); ok && v != "" {
		return v, true
	}
	if gp, ok := gopath(sc, nil); !sc.hasGoEnv() && ok {
		if list := filepath.SplitList(gp); len(list) > 0 && list[0] != "" {
			return filepath.Join(list[0], "pkg", "mod"), true
		}
	}
	return "", false
}
func mktemp(sc *scan, args []*syntax.Word) (string, bool) {
	name, tmp := "", false
	for _, a := range args {
		l := a.Lit()
		short := strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--")
		switch {
		case l == "" || strings.HasPrefix(l, "--tmpdir=") || short && strings.ContainsRune(l[1:], 'p'):
			return "", false
		case l == "--tmpdir" || short && strings.ContainsRune(l[1:], 't'):
			tmp = true
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
func ofOperand(f func(*scan, string) (string, bool)) func(*scan, []*syntax.Word) (string, bool) {
	return func(sc *scan, args []*syntax.Word) (string, bool) {
		if len(args) > 0 && args[0].Lit() == "--" {
			args = args[1:]
		}
		if len(args) == 1 {
			if fields, err := sc.fields(args[0]); err == nil && len(fields) == 1 {
				return f(sc, fields[0])
			}
		}
		return "", false
	}
}
func dirname(_ *scan, p string) (string, bool) {
	sep := "/"
	if filepath.Separator == '\\' {
		sep = `/\`
	}
	vol := filepath.VolumeName(p)
	rest := strings.TrimRight(p[len(vol):], sep)
	if rest == "" && len(p) > len(vol) {
		return vol + string(filepath.Separator), true
	}
	i := strings.LastIndexAny(rest, sep)
	if i < 0 {
		return vol + ".", true
	}
	return vol + filepath.FromSlash(cmp.Or(strings.TrimRight(rest[:i], sep), string(filepath.Separator))), true
}
func (sc *scan) real(p string) (string, bool) {
	q, ok := sc.path(p, sc.dir)
	if !ok || sc.hasRemoteLink(q) {
		return "", false
	}
	r, err := filepath.EvalSymlinks(q)
	return r, err == nil && r != ""
}
