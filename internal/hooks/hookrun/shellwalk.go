package hookrun

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// How ShellPaths walks a parsed command: each simple command's words, in the role each
// plays there.

// role is how a word counts toward a command's reach.
type role uint8

const (
	plain       role = iota // an argument: a path when something is there, or a new file
	program                 // the program to run: a reach only when slashed and there
	target                  // a cd's target: where the command goes, unknown when not resolved
	folder                  // a folder option's value (-C, --git-dir): unknown when not resolved
	creates                 // a > target: when not there, its nearest folder
	data                    // a here-document's word, or a fragment of an argument: only when slashed and there
	slashedOnly             // an assignment's value, an inline script's word: only when slashed
	globbed                 // a glob's pattern, once walked (a key of examined only)
	walked                  // a folder a recursive reader reads, once walked (a key of examined only)
)

// whole reports whether a word in role r is a path as written, never an option's or an
// assignment's value.
func (r role) whole() bool { return r == program || r == target }

// needsSlash reports whether a word in role r names no path unless it is slashed.
func (r role) needsSlash() bool { return r == program || r == data || r == slashedOnly }

// mayCreate reports whether a word in role r that names nothing there may be a file the
// command creates, which reaches the nearest folder above it: a > target, or a slashed word
// that is not run, gone to or read as data.
func (r role) mayCreate(slashed bool) bool {
	return r == creates || slashed && (r == plain || r == folder || r == slashedOnly)
}

// mustResolve reports whether a word in role r whose expansion cannot be resolved reaches
// somewhere not known: a cd's target or a folder option's value always, any other that may
// be a path (slashed) unless it is only run or read as data.
func (r role) mustResolve(slashed bool) bool {
	return r == target || r == folder || slashed && r != program && r != data
}

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
				sc.arg(w, plain)
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

// call reads a simple command: its assignments, its wrappers' words, each in its role, and
// the command they run, in the folder they run it in: as its words, a script split from a
// string, or a shell script.
func (sc *scan) call(ce *syntax.CallExpr) {
	for _, a := range ce.Assigns {
		sc.assign(a)
	}
	if len(ce.Args) == 0 {
		return
	}
	u := unwrap(ce.Args)
	read := func() {
		for j, w := range u.before {
			if isAssignment(w) {
				sc.export(w)
			} else {
				sc.arg(w, u.roles[j])
			}
		}
		switch {
		case u.split != nil:
			sc.splitScript(u.split, u.command)
		case u.script != nil:
			sc.inline(u.script, true)
		case len(u.command) > 0:
			sc.run(u.command)
		}
	}
	if slices.Contains(u.roles, target) {
		sc.subshell(read) // the folder a wrapper moves to is the command's alone
	} else {
		read()
	}
}

// export reads NAME=value, a wrapper's assignment (env A=1): its slashed value is a
// candidate, and NAME holds it for the rest of the command, nil when not resolved.
func (sc *scan) export(w *syntax.Word) {
	sc.arg(w, slashedOnly)
	name, _, _ := strings.Cut(leadingLit(w), "=")
	sc.vars[name] = nil
	if fields, err := sc.fields(w); err == nil {
		_, v, _ := strings.Cut(strings.Join(fields, " "), "=")
		sc.vars[name] = &v
	}
}

// splitScript reads the command env -S runs: its string split into words, then the words
// after it, as written, read as a script in a subshell.
func (sc *scan) splitScript(s *syntax.Word, rest []*syntax.Word) {
	fields, err := sc.fields(s)
	if err != nil {
		sc.unknown = true
		return
	}
	text := strings.Join(fields, " ")
	if len(rest) > 0 {
		text += " " + sc.src[rest[0].Pos().Offset():rest[len(rest)-1].End().Offset()]
	}
	sc.subshell(func() { sc.script(text) })
}

// run reads a command past its wrappers: its program, then its arguments, a cd's or
// pushd's target, a popd, an inline script (eval's, trap's, a shell's or an interpreter's)
// and a folder option's value read as such, and below the folders a recursive reader reads.
func (sc *scan) run(args []*syntax.Word) {
	sc.arg(args[0], program)
	name, args := programName(args[0]), args[1:]
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
	s, scripted := scriptOf(name)
	r, isReader := readers[name]
	recursive, sub := isReader && r.always, ""
	var operands []*syntax.Word
	for j := 0; j < len(args); j++ {
		opt, next, after := args[j].Lit(), j+1 < len(args), ""
		if next {
			after = args[j+1].Lit()
		}
		dashCFolder := slices.Contains(dashC, name) && (name != "git" || len(operands) == 0)
		switch {
		case next && scripted && s.takes(opt):
			j++
			sc.inline(args[j:j+1], s.shell)
		case next && (slices.Contains(folderOptions, opt) || opt == "-C" && dashCFolder):
			j++
			sc.arg(args[j], folder)
		case next && len(operands) == 0 && slices.Contains(valueOptions[name], opt):
			j++
			sc.arg(args[j], plain)
		default:
			recursive = recursive || isReader && r.recursive(opt, after, j == 0)
			if !strings.HasPrefix(opt, "-") {
				if len(operands) == 0 {
					sub = opt
				}
				operands = append(operands, args[j])
			}
			sc.arg(args[j], folderValue(args[j], dashCFolder))
		}
	}
	if recursive && (r.sub == "" || r.sub == sub) {
		sc.recurse(operands)
	}
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
		sc.arg(a.Value, plain)
	default:
		sc.arg(a.Value, slashedOnly)
		sc.vars[a.Name.Value] = nil
		if fields, err := sc.fields(a.Value); err == nil {
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
			sc.arg(w, plain)
			fields, err := sc.fields(w)
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
		if fields, err := sc.fields(r.Word); err == nil {
			sc.body(strings.Join(fields, " "), shell)
		}
	case syntax.RdrIn:
		sc.arg(r.Word, plain)
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
// which run, then its text as data, expanded, or as written when it cannot be or holds
// nothing to expand.
func (sc *scan) heredoc(body *syntax.Word, shell bool) {
	if body == nil || len(body.Parts) == 0 {
		return
	}
	text := sc.src[body.Parts[0].Pos().Offset():body.Parts[len(body.Parts)-1].End().Offset()]
	if !shell && body.Lit() == "" {
		sc.substitutions(body)
		if expanded, err := sc.document(body); err == nil {
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
	sc.fragments(text, data)
}

// fragments reads the words of text, a script's or a here-document's (fragments), in role r.
func (sc *scan) fragments(text string, r role) {
	for f := range fragments(text) {
		if sc.unknown {
			return
		}
		sc.candidate(f, r)
	}
}

// inline reads words, the script a shell or an interpreter runs from its arguments: a
// shell's as commands, in a subshell, one not resolved unknown; an interpreter's slashed
// words.
func (sc *scan) inline(words []*syntax.Word, shell bool) {
	var script []string
	for _, w := range words {
		sc.substitutions(w)
		fields, err := sc.fields(w)
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
	sc.fragments(strings.Join(script, " "), slashedOnly)
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
