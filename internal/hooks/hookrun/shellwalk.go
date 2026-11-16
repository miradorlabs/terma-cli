package hookrun

import (
	"path/filepath"
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
	data                    // a here-document's word, or a fragment of prose (an argument holding whitespace): only when slashed and there
	part                    // a fragment of an argument holding none (-v ../x:/src): only when slashed and there, an operand
	slashedOnly             // an assignment's value or a fragment of one, an inline script's word: only when slashed
	globbed                 // a glob's pattern, once walked (a key of examined only)
	walked                  // a folder read below, once walked (a key of examined only)
)

// whole reports whether a word in role r is a path as written, never an option's or an
// assignment's value.
func (r role) whole() bool { return r == program || r == target }

// needsSlash reports whether a word in role r names no path unless it is slashed.
func (r role) needsSlash() bool { return r == program || r == data || r == part || r == slashedOnly }

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
	return r == target || r == folder || slashed && r != program && r != data && r != part
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
// statement runs a shell, or its output is one's input (cat <<EOF | sh). Output a shell reads
// is commands when it is echo's or printf's words or a here-document (feed); any other's is
// not known (curl ... | sh).
func (sc *scan) stmt(s *syntax.Stmt, feeds bool) {
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		sc.call(c)
		if feeds {
			sc.feed(c, s.Redirs)
		}
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			sc.stmt(c.X, runsShell(c.Y.Cmd)) // a | b | c is (a | b) | c
		} else {
			sc.stmt(c.X, feeds)
		}
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
	switch s.Cmd.(type) {
	case *syntax.CallExpr, *syntax.BinaryCmd, *syntax.Block, *syntax.TimeClause:
	default:
		sc.unknown = sc.unknown || feeds // a loop's or a subshell's output, read as commands
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

// feed reads the output of ce, with its redirects, as a shell's commands: the words echo or
// printf writes, or its here-document, which its redirects read as commands; any other
// command's output is not known.
func (sc *scan) feed(ce *syntax.CallExpr, redirs []*syntax.Redirect) {
	if slices.ContainsFunc(redirs, func(r *syntax.Redirect) bool {
		return r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc || r.Op == syntax.WordHdoc
	}) {
		return
	}
	u := unwrap(ce.Args)
	if len(u.command) == 0 {
		sc.unknown = true
		return
	}
	switch words := u.command[1:]; programName(u.command[0]) {
	case "echo":
		for len(words) > 0 && strings.HasPrefix(words[0].Lit(), "-") {
			words = words[1:]
		}
		sc.echoed(words, " ")
	case "printf":
		sc.echoed(words, "\n")
	default:
		sc.unknown = true
	}
}

// echoed reads words, which echo or printf writes, each after the last with sep between
// them, as a shell's commands, a \n among them a newline.
func (sc *scan) echoed(words []*syntax.Word, sep string) {
	var text []string
	for _, w := range words {
		fields, err := sc.fields(w)
		if err != nil {
			sc.unknown = true
			return
		}
		text = append(text, fields...)
	}
	script := strings.ReplaceAll(strings.Join(text, sep), `\n`, "\n")
	sc.subshell(func() { sc.script(script) })
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
		case u.each:
			for i := range u.script {
				sc.inline(u.script[i:i+1], true)
			}
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

// cd reads a cd's or pushd's arguments: its target, past its options, is where the command
// goes, a relative one from each folder CDPATH names when the command sets it; a cd with
// none goes home, and - goes back to where it was, already reached.
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
		sc.cdpath(args[0])
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

// cdpath reaches the folders a cd's relative target w names from those the command's CDPATH
// lists (CDPATH=.. cd acme-api), going to each that is there.
func (sc *scan) cdpath(w *syntax.Word) {
	cdpath := sc.vars["CDPATH"]
	if cdpath == nil || *cdpath == "" {
		return
	}
	fields, err := sc.fields(w)
	if err != nil || len(fields) != 1 || filepath.IsAbs(fields[0]) || fields[0] == "." || fields[0] == ".." ||
		strings.HasPrefix(fields[0], "./") || strings.HasPrefix(fields[0], "../") {
		return
	}
	for _, d := range filepath.SplitList(*cdpath) {
		if d != "" {
			sc.candidate(filepath.Join(d, fields[0]), target)
		}
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
