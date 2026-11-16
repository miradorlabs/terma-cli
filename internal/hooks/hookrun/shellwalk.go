package hookrun

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// How ShellPaths walks a parsed command: each simple command's words, in the role each
// plays there.

// role is how a word counts toward a command's reach.
type role uint8

const (
	command     role = 1 << iota // the program to run: a reach only when slashed and there
	target                       // a cd's target: where the command goes, unknown when not resolved
	folder                       // a folder option's value (-C, --git-dir): unknown when not resolved
	creates                      // a > target: when not there, its nearest folder
	data                         // a here-document's word: a reach only when slashed and there
	slashedOnly                  // an assignment's value, an inline script's word: only when slashed
	glob                         // a glob's pattern, once walked
)

// Commands by what they do with their words: runsNext run the command the next names;
// shells run a script from -c or their input, interpreters from -c or -e; folderOptions
// take a folder, as the next word or after their =, as -C does for dashC (for git only
// before its subcommand: git diff -C finds copies).
var (
	runsNext      = []string{"nohup", "exec", "command", "builtin", "time", "nice"}
	shells        = []string{"bash", "sh", "zsh", "dash", "ksh", "ash"}
	interpreters  = []string{"python", "python3", "node", "ruby", "perl"}
	folderOptions = []string{"--git-dir", "--work-tree", "--prefix", "--cwd", "--dir", "--directory"}
	dashC         = []string{"git", "make", "tar", "env"}
)

// stmts reads list in order; feeds is whether its output is a shell's input.
func (sc *scan) stmts(list []*syntax.Stmt, feeds bool) {
	for _, s := range list {
		if sc.unknown {
			return // the run turns Mixed whatever else the command names
		}
		sc.stmt(s, feeds)
	}
}

// stmt reads one statement, then its redirects: a here-document is commands when the
// statement runs a shell, or its output is one's input (cat <<EOF | sh).
func (sc *scan) stmt(s *syntax.Stmt, feeds bool) {
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		sc.call(c)
	case *syntax.BinaryCmd:
		pipe := c.Op == syntax.Pipe || c.Op == syntax.PipeAll
		y := c.Y
		for b, ok := y.Cmd.(*syntax.BinaryCmd); ok && b.Op == c.Op; b, ok = y.Cmd.(*syntax.BinaryCmd) {
			y = b.Y // the pipeline's last command
		}
		sc.stmt(c.X, pipe && (feeds || runsShell(y.Cmd)))
		sc.stmt(c.Y, feeds)
	case *syntax.Subshell:
		sc.subshell(func() { sc.stmts(c.Stmts, false) })
	case *syntax.Block:
		sc.stmts(c.Stmts, feeds)
	case *syntax.IfClause:
		for ; c != nil; c = c.Else {
			sc.stmts(c.Cond, false)
			sc.stmts(c.Then, false)
		}
	case *syntax.WhileClause:
		sc.stmts(c.Cond, false)
		sc.stmts(c.Do, false)
	case *syntax.ForClause:
		sc.loop(c)
	case *syntax.CaseClause:
		sc.substitutions(c.Word)
		for _, item := range c.Items {
			sc.stmts(item.Stmts, false)
		}
	case *syntax.FuncDecl:
		sc.stmt(c.Body, false) // most functions defined are called
	case *syntax.DeclClause:
		for _, a := range c.Args {
			sc.assign(a)
		}
	case *syntax.TimeClause:
		if c.Stmt != nil {
			sc.stmt(c.Stmt, feeds)
		}
	case *syntax.CoprocClause:
		sc.stmt(c.Stmt, false)
	case *syntax.TestClause:
		syntax.Walk(c.X, func(n syntax.Node) bool {
			w, ok := n.(*syntax.Word)
			if ok {
				sc.arg(w, 0)
			}
			return !ok
		})
	}
	for _, r := range s.Redirs {
		sc.redirect(r, feeds || runsShell(s.Cmd))
	}
}

// subshell runs f in a subshell: a cd in it ends with it.
func (sc *scan) subshell(f func()) {
	dir, prev, pushed := sc.dir, sc.prev, slices.Clone(sc.pushed)
	f()
	sc.dir, sc.prev, sc.pushed = dir, prev, pushed
}

// call reads a simple command: its assignments, its name, and its arguments, a cd's or
// pushd's target, a popd, an inline script (eval's, trap's) and a folder option's value
// read as such.
func (sc *scan) call(ce *syntax.CallExpr) {
	for _, a := range ce.Assigns {
		sc.assign(a)
	}
	if len(ce.Args) == 0 {
		return
	}
	name, i := commandName(ce)
	for _, w := range ce.Args[:i+1] {
		sc.arg(w, command)
	}
	args := ce.Args[i+1:]
	switch name {
	case "cd", "pushd":
		sc.cd(args, name == "pushd")
		return
	case "popd":
		if n := len(sc.pushed); n > 0 {
			sc.prev, sc.dir, sc.pushed = sc.dir, sc.pushed[n-1], sc.pushed[:n-1]
		}
		return
	case "eval":
		sc.inline(args, true)
		return
	case "trap":
		sc.inline(args[:min(len(args), 1)], true)
		return
	}
	shell, interpreter := slices.Contains(shells, name), slices.Contains(interpreters, name)
	operands := 0
	for j := 0; j < len(args); j++ {
		opt, next := args[j].Lit(), j+1 < len(args)
		dashCFolder := slices.Contains(dashC, name) && (name != "git" || operands == 0)
		switch {
		case next && (shell && scriptFlag(opt, "c") || interpreter && scriptFlag(opt, "ce")):
			j++
			sc.inline(args[j:j+1], shell)
		case next && (slices.Contains(folderOptions, opt) || opt == "-C" && dashCFolder):
			j++
			sc.arg(args[j], folder)
		default:
			if !strings.HasPrefix(opt, "-") {
				operands++
			}
			sc.arg(args[j], folderValue(args[j], dashCFolder))
		}
	}
}

// commandName is the base name of the program ce runs, past nohup, exec and the like and
// their options, and the index of the word naming it; "" when that is no plain word.
func commandName(ce *syntax.CallExpr) (string, int) {
	i := 0
	for i < len(ce.Args)-1 && slices.Contains(runsNext, ce.Args[i].Lit()) {
		for i++; i < len(ce.Args)-1 && strings.HasPrefix(ce.Args[i].Lit(), "-"); i++ {
		}
	}
	if name := ce.Args[i].Lit(); name != "" {
		return strings.TrimSuffix(filepath.Base(name), ".exe"), i
	}
	return "", i
}

// scriptFlag reports whether opt is a short option cluster ending in one of letters (-c,
// -lc, -ne), whose script is the next word.
func scriptFlag(opt, letters string) bool {
	return len(opt) >= 2 && len(opt) <= 4 && opt[0] == '-' && strings.ContainsRune(letters, rune(opt[len(opt)-1])) &&
		!strings.ContainsFunc(opt[1:], func(r rune) bool { return r < 'a' || r > 'z' })
}

// folderValue is folder for a word naming a folder after its option (--dir=x, or -Cx where
// -C takes a folder), else 0.
func folderValue(w *syntax.Word, dashCFolder bool) role {
	if lead, ok := w.Parts[0].(*syntax.Lit); ok {
		opt, _, eq := strings.Cut(lead.Value, "=")
		if eq && slices.Contains(folderOptions, opt) || dashCFolder && strings.HasPrefix(opt, "-C") && w.Lit() != "-C" {
			return folder
		}
	}
	return 0
}

// runsShell reports whether cmd runs a shell reading its commands from its input: no -c.
func runsShell(cmd syntax.Command) bool {
	ce, ok := cmd.(*syntax.CallExpr)
	if !ok || len(ce.Args) == 0 {
		return false
	}
	name, i := commandName(ce)
	return slices.Contains(shells, name) && !slices.ContainsFunc(ce.Args[i+1:], func(w *syntax.Word) bool { return scriptFlag(w.Lit(), "c") })
}

// cd reads a cd's or pushd's arguments: its target, past its options, is where the command
// goes; a cd with none goes home, and - goes back to where it was, already reached.
func (sc *scan) cd(args []*syntax.Word, push bool) {
	for len(args) > 0 && len(args[0].Lit()) > 1 && args[0].Lit()[0] == '-' {
		args = args[1:]
	}
	from := sc.dir
	switch {
	case len(args) > 0 && args[0].Lit() == "-":
		if sc.prev != "" {
			sc.prev, sc.dir = sc.dir, sc.prev
		}
	case len(args) > 0:
		sc.arg(args[0], target)
	case !push:
		if home, ok := sc.lookup("HOME"); ok {
			sc.candidate(home, target)
		}
	}
	if push && sc.dir != from {
		sc.pushed = append(sc.pushed, from)
	}
}

// assign reads NAME=value: a slashed value is a candidate, and NAME holds it for the rest
// of the command, nil when not resolved.
func (sc *scan) assign(a *syntax.Assign) {
	switch {
	case a.Value == nil:
	case a.Name == nil || a.Naked:
		sc.arg(a.Value, 0)
	default:
		sc.arg(a.Value, slashedOnly)
		sc.vars[a.Name.Value] = nil
		if fields, err := expand.Fields(sc.cfg, a.Value); err == nil {
			v := strings.Join(fields, " ")
			sc.vars[a.Name.Value] = &v
		}
	}
}

// loop reads for NAME in WORDS; do BODY; done: WORDS are candidates as any command's, and
// BODY is read once for each, NAME bound to it in turn, each pass charged the body's
// statements (with no in, NAME is not known). A C-style loop's body is read once.
func (sc *scan) loop(fc *syntax.ForClause) {
	iter, ok := fc.Loop.(*syntax.WordIter)
	if !ok {
		sc.stmts(fc.Do, false)
		return
	}
	values := []*string{nil}
	if iter.InPos.IsValid() {
		values = nil
		for _, w := range iter.Items {
			sc.arg(w, 0)
			fields, err := expand.Fields(sc.cfg, w)
			if err != nil {
				values = append(values, nil)
			}
			for _, f := range fields {
				values = append(values, &f)
			}
		}
	}
	for _, v := range values {
		if sc.vars[iter.Name.Value] = v; !sc.spend(len(fc.Do)) {
			return
		}
		sc.stmts(fc.Do, false)
	}
}

// redirect reads a redirect's target, a file read or one the command may create but no
// file descriptor (2>&1), or a here-document's body.
func (sc *scan) redirect(r *syntax.Redirect, shell bool) {
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		sc.heredoc(r.Hdoc, shell)
	case syntax.WordHdoc:
		sc.substitutions(r.Word)
		if fields, err := expand.Fields(sc.cfg, r.Word); err == nil {
			sc.body(strings.Join(fields, " "), shell)
		}
	case syntax.RdrIn:
		sc.arg(r.Word, 0)
	case syntax.DplIn, syntax.DplOut:
		if l := r.Word.Lit(); l == "-" || strings.Trim(l, "0123456789") == "" {
			return
		}
		fallthrough
	default:
		sc.arg(r.Word, creates)
	}
}

// heredoc reads a here-document's body: for a shell as written; else its substitutions,
// which run, then its text as data, expanded, or as written when it cannot be.
func (sc *scan) heredoc(body *syntax.Word, shell bool) {
	if body == nil || len(body.Parts) == 0 {
		return
	}
	text := sc.src[body.Parts[0].Pos().Offset():body.Parts[len(body.Parts)-1].End().Offset()]
	if !shell {
		sc.substitutions(body)
		if expanded, err := expand.Document(sc.cfg, body); err == nil {
			text = expanded
		}
	}
	sc.body(text, shell)
}

// body reads text fed to a command's input: commands for a shell, in a subshell; else
// data, whose slashed words reach what is there.
func (sc *scan) body(text string, shell bool) {
	if shell {
		sc.subshell(func() { sc.script(text) })
		return
	}
	for f := range fragments(text) {
		if sc.unknown {
			return
		}
		sc.candidate(f, data)
	}
}

// inline reads words, the script a shell or an interpreter runs from its arguments: a
// shell's as commands, in a subshell, one not resolved unknown; an interpreter's slashed
// words.
func (sc *scan) inline(words []*syntax.Word, shell bool) {
	var script []string
	for _, w := range words {
		sc.substitutions(w)
		fields, err := expand.Fields(sc.cfg, w)
		if err != nil {
			sc.unknown = sc.unknown || shell || slashed(w)
			return
		}
		script = append(script, fields...)
	}
	if shell {
		sc.subshell(func() { sc.script(strings.Join(script, " ")) })
		return
	}
	for f := range fragments(strings.Join(script, " ")) {
		sc.candidate(f, slashedOnly)
	}
}

// substitutions reads the commands of the command and process substitutions in w, which
// run before it. A cd in one is taken to last past it, as a cd in a branch may.
func (sc *scan) substitutions(w *syntax.Word) {
	if w == nil {
		return
	}
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst:
			sc.stmts(n.Stmts, false)
			return false
		case *syntax.ProcSubst:
			sc.stmts(n.Stmts, false)
			return false
		}
		return true
	})
}
