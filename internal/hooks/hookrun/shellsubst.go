package hookrun

import (
	"cmp"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// outputs maps known command-substitution prefixes to static path evaluators.
var outputs = map[string]func(sc *scan, args []*syntax.Word) (string, bool){
	"pwd": where, "pwd -L": where, "pwd -P": whereP, "git rev-parse --show-toplevel": toplevel,
	"go env GOPATH": gopath, "go env GOMODCACHE": modcache, "mktemp": mktemp,
	"dirname":  ofOperand(dirname),
	"basename": ofOperand(basename),
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
	if sc.dir != "" && len(args) == 0 {
		r, ok := sc.real(sc.dir)
		return r, ok && !sc.unknown
	}
	return "", false
}
func toplevel(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 {
		return "", false
	}
	return sc.safeLocate(sc.dir)
}
func (sc *scan) hasGoEnv(key string) bool {
	if sc.isUnresolvedVar("GOENV") {
		return true
	}
	p, _ := sc.lookup("GOENV")
	if p == "off" {
		return false
	}
	if p == "" {
		dir := ""
		if runtime.GOOS == "windows" {
			dir, _ = sc.lookup("AppData")
		} else if xdg, _ := sc.lookup("XDG_CONFIG_HOME"); runtime.GOOS != "darwin" && xdg != "" {
			dir = xdg
		} else if home, ok := sc.lookup("HOME"); ok {
			if dir = filepath.Join(home, ".config"); runtime.GOOS == "darwin" {
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
	if data, err := readSmallFile(p); err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	} else {
		return slices.ContainsFunc(strings.Split(data, "\n"), func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), key+"=") })
	}
}
func gopath(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOPATH") {
		return "", false
	}
	if v, ok := sc.lookup("GOPATH"); ok && v != "" {
		return v, true
	}
	key := "HOME"
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	}
	home, ok := sc.lookup(key)
	return filepath.Join(home, "go"), !sc.isUnresolvedVar(key) && !sc.hasGoEnv("GOPATH") && ok && home != ""
}
func modcache(sc *scan, args []*syntax.Word) (string, bool) {
	if len(args) != 0 || sc.isUnresolvedVar("GOMODCACHE") {
		return "", false
	}
	if v, ok := sc.lookup("GOMODCACHE"); ok && v != "" {
		return v, true
	}
	if gp, ok := gopath(sc, nil); !sc.hasGoEnv("GOMODCACHE") && ok {
		if first, _, _ := strings.Cut(gp, string(os.PathListSeparator)); first != "" {
			return filepath.Join(first, "pkg", "mod"), true
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
		case l == "" || strings.HasPrefix(l, "--tmpdir=") || short && strings.ContainsRune(l[1:], 'p') || !strings.HasPrefix(l, "-") && pathLike(l):
			return "", false
		case l == "--tmpdir" || short && strings.ContainsRune(l[1:], 't'):
			tmp = true
		case !strings.HasPrefix(l, "-"):
			name = l
		}
	}
	dir := sc.dir
	if name == "" || tmp {
		if sc.isUnresolvedVar("TMPDIR") {
			return "", false
		}
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
func basename(_ *scan, p string) (string, bool) {
	if p != "" {
		p = filepath.Base(p)
	}
	return p, true
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
	} else if i := strings.LastIndexAny(rest, sep); i < 0 {
		return vol + ".", true
	} else {
		return vol + filepath.FromSlash(cmp.Or(strings.TrimRight(rest[:i], sep), string(filepath.Separator))), true
	}
}
func (sc *scan) real(p string) (string, bool) {
	q, ok := sc.path(p, sc.dir)
	if !ok || sc.hasRemoteLink(q) {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(q); err == nil {
		return r, r != ""
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if _, err := os.Lstat(q); errors.Is(err, fs.ErrNotExist) && filepath.Dir(q) != q && !sc.hasRemoteLink(filepath.Dir(q)) {
		if pr, err := filepath.EvalSymlinks(filepath.Dir(q)); err == nil && pr != "" && !remote(pr, sc.cwd) {
			info, err := os.Stat(pr)
			return filepath.Join(pr, filepath.Base(q)), err == nil && info.IsDir()
		}
	}
	return "", false
}
