package hookrun

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
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

// stmt reads one statement and its redirects; backgrounded commands and pipeline stages run in subshells.
func (sc *scan) stmt(s *syntax.Stmt, feeds bool) {
	if sc.dirUncertain {
		sc.unknown = true
		return
	}
	if s.Background {
		defer sc.restore(sc.snapshot())
	}
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		if sc.call(c); feeds {
			sc.feed(c, s.Redirs)
		}
	case *syntax.BinaryCmd:
		if c.Op != syntax.Pipe && c.Op != syntax.PipeAll {
			parts := append(chain(c.X, c.Op), chain(c.Y, c.Op)...)
			states := make([]branchState, 0, len(parts))
			for _, p := range parts {
				if sc.stmt(p, feeds); sc.unknown {
					return
				}
				states = append(states, sc.snapshot())
			}
			sc.mergeBranches(states)
		} else {
			var buf strings.Builder
			run, prev := runsShell(c.Y.Cmd), sc.feedBuf
			if run {
				sc.feedBuf = &buf
			}
			sc.subshell(func() { sc.stmt(c.X, run) })
			if sc.feedBuf = prev; run && !sc.unknown && buf.Len() > 0 {
				sc.subshell(func() { sc.script(buf.String()) })
			}
			sc.subshell(func() { sc.stmt(c.Y, feeds) })
		}
	case *syntax.Subshell:
		sc.subshell(func() { sc.stmts(c.Stmts, false) })
	case *syntax.Block:
		sc.stmts(c.Stmts, feeds)
	case *syntax.IfClause:
		sc.ifClause(c)
	case *syntax.WhileClause:
		sc.stmts(c.Cond, false)
		before := sc.snapshot()
		sc.stmts(c.Do, false)
		sc.mergeBranches([]branchState{before, sc.snapshot()})
		sc.unknown = sc.unknown || sc.dirUncertain
	case *syntax.ForClause:
		sc.loop(c)
	case *syntax.CaseClause:
		sc.caseClause(c)
	case *syntax.FuncDecl:
		sc.stmt(c.Body, false)
	case *syntax.DeclClause:
		for _, a := range c.Args {
			sc.assign(a)
		}
	case *syntax.TimeClause:
		if c.Stmt != nil {
			sc.stmt(c.Stmt, feeds)
		}
	case *syntax.CoprocClause:
		sc.subshell(func() { sc.stmt(c.Stmt, false) })
	case *syntax.TestClause:
		syntax.Walk(c.X, func(n syntax.Node) bool {
			w, ok := n.(*syntax.Word)
			if ok {
				sc.arg(w, plain)
			}
			return !ok
		})
	default:
		sc.substitutions(s.Cmd)
	}
	switch s.Cmd.(type) {
	case *syntax.CallExpr, *syntax.BinaryCmd, *syntax.Block, *syntax.TimeClause:
	default:
		sc.unknown = sc.unknown || feeds
	}
	for _, r := range s.Redirs {
		sc.redirect(r, feeds || runsShell(s.Cmd))
	}
}
func (sc *scan) subshell(f func()) { saved := sc.snapshot(); f(); sc.restore(saved) }
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
	words, sep, printf := u.command[1:], " ", false
	switch programName(u.command[0]) {
	case "echo":
		for len(words) > 0 && strings.HasPrefix(words[0].Lit(), "-") {
			words = words[1:]
		}
	case "printf":
		sep, printf = "\n", true
	default:
		sc.unknown = true
		return
	}
	var text []string
	for i, w := range words {
		fields, err := sc.fields(w)
		if err != nil || printf && i == 0 && slices.ContainsFunc(fields, func(f string) bool { return strings.ContainsRune(f, '%') }) {
			sc.unknown = true
			return
		}
		text = append(text, fields...)
	}
	sc.body(strings.ReplaceAll(strings.Join(text, sep), `\n`, "\n"), true)
}
func (sc *scan) call(ce *syntax.CallExpr) {
	if len(ce.Args) == 0 {
		for _, a := range ce.Assigns {
			sc.assign(a)
		}
		return
	}
	u := unwrap(ce.Args)
	if len(ce.Assigns) > 0 || slices.ContainsFunc(u.before, isAssignment) {
		savedVars, savedLoops := maps.Clone(sc.vars), maps.Clone(sc.loopVars)
		defer func() { sc.vars, sc.loopVars = savedVars, savedLoops }()
	}
	for _, a := range ce.Assigns {
		sc.assign(a)
	}
	if slices.Contains(u.roles, target) || len(u.attachedChdir) > 0 {
		defer sc.restore(sc.snapshot())
	}
	for j, w := range u.before {
		if isAssignment(w) {
			sc.export(w)
		} else if opt, ok := u.attachedChdir[j]; ok {
			sc.arg(w, folder)
			if fields, err := sc.fields(w); err != nil || len(fields) != 1 {
				sc.unknown = true
			} else if strings.HasPrefix(opt, "--") {
				sc.intoPath(strings.TrimPrefix(fields[0], opt+"="))
			} else {
				sc.intoPath(strings.TrimPrefix(fields[0], opt))
			}
		} else {
			sc.arg(w, u.roles[j])
		}
	}
	switch {
	case u.split != nil:
		fields, err := sc.fields(u.split)
		if err != nil {
			sc.unknown = true
			return
		}
		text := strings.Join(fields, " ")
		if len(u.command) > 0 {
			text += " " + sc.src[u.command[0].Pos().Offset():u.command[len(u.command)-1].End().Offset()]
		}
		sc.subshell(func() { sc.script(text) })
	case u.each:
		for i := range u.script {
			sc.inline(u.script[i:i+1], true, true)
		}
	case u.script != nil:
		sc.inline(u.script, true, true)
	case len(u.command) > 0:
		sc.run(u.command)
	}
}
func (sc *scan) setVar(name string, val *string) { sc.vars[name] = val; delete(sc.loopVars, name) }
func (sc *scan) export(w *syntax.Word) {
	sc.arg(w, slashedOnly)
	name, _, _ := strings.Cut(leadingLit(w), "=")
	sc.setVar(name, nil)
	if fields, err := sc.fields(w); err == nil {
		_, v, _ := strings.Cut(strings.Join(fields, " "), "=")
		sc.vars[name] = &v
	}
}
func (sc *scan) cd(args []*syntax.Word, push bool) {
	for len(args) > 0 && len(args[0].Lit()) > 1 && args[0].Lit()[0] == '-' {
		args = args[1:]
	}
	from, pushed := sc.dir, false
	switch {
	case len(args) > 0 && args[0].Lit() == "-":
		if sc.prev != "" {
			sc.prev, sc.dir, pushed = sc.dir, sc.prev, true
		}
	case len(args) > 0:
		sc.cdpath(args[0])
		sc.arg(args[0], target)
		if pushed = sc.dir != from; !pushed {
			if fields, err := sc.fields(args[0]); err == nil && len(fields) == 1 {
				p, ok := sc.path(fields[0], sc.dir)
				pushed = ok && p == sc.dir
			}
		}
	case push:
		if n := len(sc.pushed); n > 0 {
			sc.dir, sc.pushed[n-1] = sc.pushed[n-1], sc.dir
		}
	default:
		if home, ok := sc.lookup("HOME"); ok {
			sc.candidate(home, target)
		} else if _, set := sc.vars["HOME"]; set {
			sc.unknown = true
		}
	}
	if push && pushed {
		sc.pushed = append(sc.pushed, from)
	}
}
func (sc *scan) cdpath(w *syntax.Word) {
	cdpath, ok := sc.lookup("CDPATH")
	_, uncertain := sc.vars["CDPATH"]
	if !ok && !uncertain || ok && cdpath == "" {
		return
	}
	fields, err := sc.fields(w)
	if err != nil || len(fields) != 1 || filepath.IsAbs(fields[0]) || fields[0] == "." || fields[0] == ".." ||
		strings.HasPrefix(fields[0], "./") || strings.HasPrefix(fields[0], "../") {
		return
	}
	if !ok {
		sc.unknown = true
	} else {
		for _, d := range filepath.SplitList(cdpath) {
			if d != "" {
				sc.candidate(filepath.Join(d, fields[0]), target)
			}
		}
	}
}
func (sc *scan) assign(a *syntax.Assign) {
	if sc.substitutions(a.Index); a.Array != nil {
		for _, elem := range a.Array.Elems {
			if sc.substitutions(elem.Index); elem.Value != nil {
				sc.arg(elem.Value, plain)
			}
		}
		if a.Name != nil {
			sc.setVar(a.Name.Value, nil)
		}
		return
	}
	if a.Value == nil || a.Name == nil || a.Naked {
		if a.Value != nil {
			sc.arg(a.Value, plain)
		}
		return
	}
	sc.arg(a.Value, slashedOnly)
	prior, hadPrior := sc.lookup(a.Name.Value)
	sc.setVar(a.Name.Value, nil)
	if fields, err := sc.fields(a.Value); err == nil {
		v := strings.Join(fields, " ")
		if a.Append && hadPrior {
			v = prior + v
		}
		sc.vars[a.Name.Value] = &v
	}
}
func (sc *scan) loop(fc *syntax.ForClause) {
	iter, ok := fc.Loop.(*syntax.WordIter)
	if !ok {
		sc.substitutions(fc.Loop)
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
	name, prevLoop := iter.Name.Value, sc.loopVars[iter.Name.Value]
	defer func() { sc.setLoopVar(name, prevLoop) }()
	for _, v := range values {
		sc.vars[name] = v
		if sc.setLoopVar(name, v == nil); !sc.spend(len(fc.Do)) {
			return
		}
		sc.stmts(fc.Do, false)
	}
}
func (sc *scan) redirect(r *syntax.Redirect, shell bool) {
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		if b := r.Hdoc; b != nil && len(b.Parts) > 0 {
			text := sc.src[b.Parts[0].Pos().Offset():b.Parts[len(b.Parts)-1].End().Offset()]
			if !shell && b.Lit() == "" {
				sc.substitutions(b)
				if expanded, err := sc.expandWord(b, expand.Document); err == nil {
					text = expanded
				}
			}
			sc.body(text, shell)
		}
	case syntax.WordHdoc:
		sc.substitutions(r.Word)
		if fields, err := sc.fields(r.Word); err == nil {
			sc.body(strings.Join(fields, " "), shell)
		} else if shell {
			sc.unknown = true
		}
	case syntax.RdrIn, syntax.RdrInOut:
		sc.arg(r.Word, plain)
		sc.unknown = sc.unknown || shell
	case syntax.DplIn, syntax.DplOut:
		if l := r.Word.Lit(); l != "-" && strings.Trim(l, "0123456789") != "" {
			sc.arg(r.Word, creates)
		}
	default:
		sc.arg(r.Word, creates)
	}
}
func (sc *scan) body(text string, shell bool) {
	if !shell {
		sc.fragments(text, data)
	} else if sc.feedBuf != nil && sc.charge(len(text)+1) {
		sc.feedBuf.WriteString(text + "\n")
	} else if sc.feedBuf == nil {
		sc.subshell(func() { sc.script(text) })
	}
}
func (sc *scan) fragments(text string, r role) {
	for f := range fragments(text) {
		if sc.unknown {
			return
		}
		sc.candidate(f, r)
	}
}
func (sc *scan) withoutFeed(f func()) { prev := sc.feedBuf; sc.feedBuf = nil; f(); sc.feedBuf = prev }
func (sc *scan) inline(words []*syntax.Word, shell, isolate bool) {
	sc.withoutFeed(func() {
		var script []string
		for _, w := range words {
			sc.substitutions(w)
			fields, err := sc.fields(w)
			if err != nil {
				sc.unknown = sc.unknown || shell || sc.mustResolveWord(w)
				return
			}
			script = append(script, fields...)
		}
		text := strings.Join(script, " ")
		if !shell {
			sc.fragments(text, slashedOnly)
		} else if isolate {
			sc.subshell(func() { sc.script(text) })
		} else {
			sc.script(text)
		}
	})
}
func chain(s *syntax.Stmt, op syntax.BinCmdOperator) []*syntax.Stmt {
	if len(s.Redirs) == 0 && !s.Background && !s.Negated {
		if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && b.Op == op {
			return append(chain(b.X, op), chain(b.Y, op)...)
		}
	}
	return []*syntax.Stmt{s}
}
func (sc *scan) substitutions(node syntax.Node) {
	if node == nil {
		return
	}
	sc.withoutFeed(func() {
		syntax.Walk(node, func(n syntax.Node) bool {
			switch x := n.(type) {
			case *syntax.CmdSubst:
				sc.subshell(func() { sc.stmts(x.Stmts, false) })
			case *syntax.ProcSubst:
				sc.subshell(func() { sc.stmts(x.Stmts, false) })
			default:
				return true
			}
			return false
		})
	})
}
