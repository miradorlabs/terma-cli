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
	for i := 0; i < len(list) && !sc.unknown; i++ {
		sc.stmt(list[i], feeds)
	}
}

// stmt reads one statement and its redirects; backgrounded commands and pipeline stages run in subshells.
func (sc *scan) stmt(s *syntax.Stmt, feeds bool) {
	if sc.dirUncertain && sc.fail() {
		return
	} else if s.Background {
		defer sc.restore(sc.snapshot())
	}
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		if sc.call(c); feeds {
			sc.feed(c, s.Redirs)
		}
	case *syntax.BinaryCmd:
		if c.Op != syntax.Pipe && c.Op != syntax.PipeAll {
			parts, states := append(chain(c.X, c.Op), chain(c.Y, c.Op)...), []branchState(nil)
			for i := 0; i < len(parts) && !sc.unknown; i++ {
				sc.stmt(parts[i], feeds)
				states = append(states, sc.snapshot())
			}
			sc.mergeBranches(states)
		} else {
			var buf strings.Builder
			run, prev, after := runsShell(c.Y.Cmd), sc.feedBuf, branchState{} // lastpipe runs the last stage in this shell
			sc.feedBuf = pick(run, &buf, sc.feedBuf)
			sc.subshell(func() { sc.stmt(c.X, run) })
			if sc.feedBuf = prev; run && !sc.unknown && buf.Len() > 0 {
				sc.subshell(func() { sc.script(buf.String()) })
			}
			sc.subshell(func() { sc.stmt(c.Y, feeds); after = sc.snapshot() })
			sc.mergeBranches(pick(strings.Contains(sc.src, "lastpipe") || strings.Contains(sc.envVal("BASHOPTS"), "lastpipe") || sc.isUnresolvedVar("BASHOPTS"), []branchState{sc.snapshot(), after}, nil))
			sc.unknown = runsXargs(c.Y.Cmd) && !sc.xargsSafeFeed(c.X) || sc.unknown // after the feed check, which may fail closed
		}
	case *syntax.Subshell:
		sc.subshell(func() { sc.stmts(c.Stmts, false) })
	case *syntax.Block:
		sc.stmts(c.Stmts, feeds)
	case *syntax.IfClause:
		sc.ifClause(c)
	case *syntax.WhileClause:
		sc.stmts(c.Cond, false)
		sc.repeatLoop(c.Do)
	case *syntax.ForClause:
		sc.loop(c)
	case *syntax.CaseClause:
		sc.caseClause(c)
	case *syntax.FuncDecl:
		sc.funcs[c.Name.Value] = c.Body
		sc.subshell(func() { sc.stmt(c.Body, false) })
	case *syntax.DeclClause:
		for _, a := range c.Args {
			if sc.assign(a); a.Naked && c.Variant != nil && slices.Contains([]string{"local", "declare", "typeset"}, c.Variant.Value) {
				if a.Name != nil {
					sc.setVar(a.Name.Value, nil)
				} else {
					sc.unknown = sc.unknown || a.Value != nil && (!strings.HasPrefix(a.Value.Lit(), "-") || strings.ContainsRune(a.Value.Lit(), 'n')) // -n: nameref
				}
			}
		}
	case *syntax.TimeClause:
		if c.Stmt != nil {
			sc.stmt(c.Stmt, feeds)
		}
	case *syntax.CoprocClause:
		sc.subshell(func() { sc.stmt(c.Stmt, false) })
	case *syntax.ArithmCmd:
		sc.substitutions(c)
		sc.taintArith(c.X)
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
		sc.unknown = sc.unknown || (r.Op == syntax.RdrIn || r.Op == syntax.RdrInOut || isHdoc(r.Op)) && runsXargs(s.Cmd)
	}
}
func isHdoc(op syntax.RedirOperator) bool {
	return op == syntax.Hdoc || op == syntax.DashHdoc || op == syntax.WordHdoc
}
func (sc *scan) subshell(f func()) { saved := sc.snapshot(); f(); sc.restore(saved) }
func (sc *scan) feed(ce *syntax.CallExpr, redirs []*syntax.Redirect) {
	u := unwrap(ce.Args)
	if slices.ContainsFunc(redirs, func(r *syntax.Redirect) bool { return isHdoc(r.Op) }) || len(u.command) == 0 && sc.fail() {
		return
	}
	name, text := programName(u.command[0]), []string(nil)
	for _, w := range u.command[1:] {
		fields, err := sc.fields(w)
		if err != nil && sc.fail() {
			return
		}
		text = append(text, fields...)
	}
	for ; name == "echo" && len(text) > 0 && slices.Contains([]string{"-n", "-E"}, text[0]); text = text[1:] {
	}
	s := strings.Join(text, pick(name == "printf", "\n", " "))
	if (name != "echo" && name != "printf" || name == "echo" && (len(text) > 0 && len(text[0]) > 1 && text[0][0] == '-' && strings.Trim(text[0][1:], "neE") == "" || strings.ContainsRune(s, '\\')) || name == "printf" && len(text) > 0 && (strings.HasPrefix(text[0], "-") || strings.ContainsRune(text[0], '%') || strings.ContainsRune(strings.ReplaceAll(text[0], `\n`, ""), '\\'))) && sc.fail() {
		return
	}
	sc.body(pick(name == "printf", strings.ReplaceAll(s, `\n`, "\n"), s), true)
}
func (sc *scan) call(ce *syntax.CallExpr) {
	u, setVars := unwrap(ce.Args), map[string]*string(nil)
	if len(ce.Args) > 0 && (len(u.envMod) > 0 || slices.ContainsFunc(u.before, isAssignment)) {
		savedEnv, savedVars, savedLoops := sc.env, maps.Clone(sc.vars), maps.Clone(sc.loopVars)
		defer func() { sc.env, sc.vars, sc.loopVars = savedEnv, savedVars, savedLoops }()
	} else if len(ce.Args) > 0 && len(ce.Assigns) > 0 {
		beforeVars, beforeLoops := maps.Clone(sc.vars), maps.Clone(sc.loopVars)
		_, name, _ := sc.command(append(slices.Clip(u.command), ce.Args...))
		setVars = make(map[string]*string, len(ce.Assigns))
		defer func() { sc.restoreAssigns(beforeVars, beforeLoops, setVars, name) }()
	}
	for _, a := range ce.Assigns {
		if sc.assign(a); setVars != nil && a.Name != nil {
			setVars[a.Name.Value] = sc.vars[a.Name.Value]
		}
	}
	if len(ce.Args) == 0 {
		return
	}
	if slices.Contains(u.roles, target) || slices.Contains(u.roles, folder) {
		defer sc.restore(sc.snapshot())
	}
	for j, w := range u.before {
		if mod, ok := u.envMod[j]; ok {
			sc.applyEnvMod(w, mod)
		} else if isAssignment(w) {
			sc.export(w)
		} else if opt, ok := u.attached[j]; ok {
			sc.substitutions(w)
			if fields, err := sc.fields(w); err != nil || len(fields) != 1 {
				sc.unknown = true
			} else {
				val := strings.TrimPrefix(fields[0], opt+pick(strings.HasPrefix(opt, "--"), "=", ""))
				if sc.candidate(val, u.roles[j]); u.roles[j] == folder {
					sc.intoPath(val)
				}
			}
		} else {
			sc.arg(w, u.roles[j])
		}
	}
	switch sc.unknown = sc.unknown || u.unknown; {
	case u.split != nil:
		if fields, err := sc.fields(u.split); err != nil {
			sc.unknown = true
		} else {
			text := strings.Join(fields, " ")
			if len(u.command) > 0 {
				text += " " + sc.src[u.command[0].Pos().Offset():u.command[len(u.command)-1].End().Offset()]
			}
			sc.subshell(func() { sc.script(text) })
		}
	case u.each:
		for i := range u.script {
			sc.inline(u.script[i:i+1], "", true, true)
		}
	case u.script != nil:
		sc.inline(u.script, u.scriptPrefix, true, true)
	case len(u.command) > 0:
		sc.run(u.command)
	}
}

func (sc *scan) restoreAssigns(before map[string]*string, loops map[string]bool, set map[string]*string, name string) {
	for k, v := range set {
		if sc.vars[k] == v && name != "export" && name != "readonly" { // which keep it; a special builtin may (POSIX), so resolve neither
			delete(sc.vars, k)
			if old, ok := before[k]; ok {
				sc.vars[k] = old
			}
			if sc.setLoopVar(k, loops[k]); name == "" || slices.Contains([]string{"declare", "typeset", "local", "eval", "exec", "set", ":", ".", "source", "unset", "shift", "return", "trap", "exit", "break", "continue", "times"}, name) {
				sc.setVar(k, nil)
			}
		}
	}
}
func (sc *scan) applyEnvMod(w *syntax.Word, mod string) {
	if mod == "-i" {
		sc.env, sc.vars, sc.loopVars = func(string) (string, bool) { return "", false }, map[string]*string{}, map[string]bool{}
	} else if sc.substitutions(w); true {
		if fields, err := sc.fields(w); err != nil || len(fields) != 1 || !isName(strings.TrimPrefix(fields[0], mod)) {
			sc.unknown = true
		} else {
			sc.setVar(strings.TrimPrefix(fields[0], mod), unsetVar)
		}
	}
}
func (sc *scan) setVar(name string, val *string) { sc.vars[name] = val; delete(sc.loopVars, name) }
func (sc *scan) export(w *syntax.Word) {
	sc.arg(w, slashedOnly)
	name, _, _ := strings.Cut(leadingLit(w), "=")
	fields, err := sc.fields(w)
	_, v, _ := strings.Cut(strings.Join(fields, " "), "=")
	sc.setVar(name, pick(err == nil, &v, nil))
}
func (sc *scan) cd(args []*syntax.Word, push bool) {
	from, pushed, r := sc.dir, false, target
	for ; len(args) > 0 && (len(args[0].Lit()) > 1 && args[0].Lit()[0] == '-' && args[0].Lit() != "--" || len(args) > 1 && strings.EqualFold(args[0].Lit(), "/d")); args = args[1:] {
		if l := args[0].Lit(); !push && l[0] == '-' && (strings.ContainsRune(l, 'P') || strings.ContainsRune(l, 'L')) {
			r = pick(strings.LastIndexByte(l, 'P') > strings.LastIndexByte(l, 'L'), physTarget, target)
		}
	}
	args = args[pick(len(args) > 0 && args[0].Lit() == "--", 1, 0):]
	switch {
	case len(args) > 0 && args[0].Lit() == "-":
		if sc.prev != "" {
			sc.prev, sc.dir, pushed = sc.dir, sc.prev, true
		}
	case len(args) > 0:
		sc.cdpath(args[0])
		sc.arg(args[0], r)
		if pushed = sc.dir != from; !pushed {
			if fields, err := sc.fields(args[0]); err == nil && len(fields) == 1 {
				p, ok := sc.resolvePath(fields[0], sc.dir, r == target)
				pushed = ok && p == sc.dir
			}
		}
	case push:
		if n := len(sc.pushed); n > 0 {
			sc.dir, sc.pushed[n-1] = sc.pushed[n-1], sc.dir
		}
	default:
		if home, ok := sc.lookup("HOME"); ok {
			sc.candidate(home, r)
		} else {
			sc.unknown = sc.unknown || sc.isUnresolvedVar("HOME")
		}
	}
	if push && pushed {
		sc.pushed = append(sc.pushed, from)
	}
}
func (sc *scan) cdpath(w *syntax.Word) {
	if cdpath, ok := sc.lookup("CDPATH"); ok && cdpath != "" || !ok && sc.isUnresolvedVar("CDPATH") {
		if fields, err := sc.fields(w); err == nil && len(fields) == 1 && !filepath.IsAbs(fields[0]) && fields[0] != "." && fields[0] != ".." && !strings.HasPrefix(fields[0], "./") && !strings.HasPrefix(fields[0], "../") {
			for _, d := range pick(ok, filepath.SplitList(cdpath), nil) {
				if d != "" {
					sc.candidate(filepath.Join(d, fields[0]), target)
				}
			}
			sc.unknown = sc.unknown || !ok
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
	} else if a.Name == nil || a.Naked {
		if a.Value != nil {
			sc.arg(a.Value, plain)
		}
	} else if a.Value == nil && !a.Append {
		sc.setVar(a.Name.Value, new(string))
	} else if a.Value != nil {
		sc.arg(a.Value, slashedOnly)
		prior, hadPrior := sc.lookup(a.Name.Value)
		fields, err := sc.fields(a.Value)
		v := pick(a.Append && hadPrior, prior, "") + strings.Join(fields, " ")
		sc.setVar(a.Name.Value, pick(err == nil && (!a.Append || !sc.isUnresolvedVar(a.Name.Value)), &v, nil))
	}
}
func (sc *scan) repeatLoop(do []*syntax.Stmt) {
	before := sc.snapshot()
	for range 2 {
		sc.stmts(do, false)
		if sc.mergeBranches([]branchState{before, sc.snapshot()}); sc.unknown || maps.Equal(before.vars, sc.vars) {
			break
		}
	}
	sc.unknown = sc.unknown || sc.dirUncertain
}
func (sc *scan) loop(fc *syntax.ForClause) {
	iter, ok := fc.Loop.(*syntax.WordIter)
	if !ok {
		sc.substitutions(fc.Loop)
		sc.taintArith(fc.Loop)
		sc.repeatLoop(fc.Do)
		return
	}
	values := pick(iter.InPos.IsValid(), []*string(nil), []*string{nil}) // no in: the positional parameters
	for _, w := range iter.Items {
		sc.arg(w, plain)
		if fields, err := sc.fields(w); err != nil {
			values = append(values, nil)
		} else {
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
			if b.Lit() == "" {
				sc.substitutions(b)
				if expanded, err := sc.expandWord(b, expand.Document); err == nil {
					text = expanded
				} else if shell && sc.fail() {
					return
				}
			}
			sc.body(text, shell)
		}
	case syntax.WordHdoc:
		sc.substitutions(r.Word)
		if fields, err := sc.fields(r.Word); err == nil {
			sc.body(strings.Join(fields, " "), shell)
		} else {
			sc.unknown = sc.unknown || shell
		}
	case syntax.RdrIn, syntax.RdrInOut:
		sc.arg(r.Word, plain)
		sc.unknown = sc.unknown || shell
	default:
		if l := r.Word.Lit(); r.Op != syntax.DplIn && r.Op != syntax.DplOut || l != "-" && strings.Trim(l, "0123456789") != "" {
			sc.arg(r.Word, creates)
		}
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
		if !sc.unknown {
			sc.candidate(f, r)
		}
	}
}
func (sc *scan) withoutFeed(f func()) { prev := sc.feedBuf; sc.feedBuf = nil; f(); sc.feedBuf = prev }
func (sc *scan) inline(words []*syntax.Word, trim string, shell, isolate bool) {
	sc.withoutFeed(func() {
		var script []string
		for _, w := range words {
			sc.substitutions(w)
			fields, err := sc.fields(w)
			if err != nil || trim != "" && len(fields) == 0 {
				sc.unknown = sc.unknown || shell || sc.mustResolveWord(w)
				return
			}
			script = append(script, fields...)
		}
		if text := strings.TrimPrefix(strings.Join(script, " "), trim); !shell {
			sc.fragments(text, slashedOnly)
		} else if isolate {
			sc.subshell(func() { sc.script(text) })
		} else {
			sc.script(text)
		}
	})
}
func chain(s *syntax.Stmt, op syntax.BinCmdOperator) []*syntax.Stmt {
	if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && b.Op == op && len(s.Redirs) == 0 && !s.Background && !s.Negated {
		return append(chain(b.X, op), chain(b.Y, op)...)
	}
	return []*syntax.Stmt{s}
}
func (sc *scan) substitutions(node syntax.Node) {
	if node != nil {
		sc.withoutFeed(func() {
			syntax.Walk(node, func(n syntax.Node) bool {
				if x, ok := n.(*syntax.CmdSubst); ok {
					sc.subshell(func() { sc.stmts(x.Stmts, false) })
				} else if x, ok := n.(*syntax.ProcSubst); ok {
					sc.subshell(func() { sc.stmts(x.Stmts, false) })
				} else {
					return true
				}
				return false
			})
		})
	}
}
