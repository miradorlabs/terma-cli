package hookrun

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"

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
	if s.Background {
		sc.subshell(func() { sc.stmtBody(s, feeds) })
		return
	}
	sc.stmtBody(s, feeds)
}

func (sc *scan) stmtBody(s *syntax.Stmt, feeds bool) {
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		sc.call(c)
		if feeds {
			sc.feed(c, s.Redirs)
		}
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			if runsShell(c.Y.Cmd) {
				var buf strings.Builder
				prev := sc.feedBuf
				sc.feedBuf = &buf
				sc.subshell(func() { sc.stmt(c.X, true) })
				sc.feedBuf = prev
				if !sc.unknown && buf.Len() > 0 {
					sc.subshell(func() { sc.script(buf.String()) })
				}
			} else {
				sc.subshell(func() { sc.stmt(c.X, false) })
			}
			sc.subshell(func() { sc.stmt(c.Y, feeds) })
		} else {
			sc.stmt(c.X, feeds)
			sc.stmt(c.Y, feeds)
		}
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
			for _, pat := range item.Patterns {
				sc.substitutions(pat)
			}
			sc.stmts(item.Stmts, false)
		}
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
			if w, ok := n.(*syntax.Word); ok {
				sc.arg(w, plain)
				return false
			}
			return true
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

func (sc *scan) subshell(f func()) {
	dir, prev, pushed, vars := sc.dir, sc.prev, slices.Clone(sc.pushed), maps.Clone(sc.vars)
	f()
	sc.dir, sc.prev, sc.pushed, sc.vars = dir, prev, pushed, vars
}

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
		sc.echoed(words, " ", false)
	case "printf":
		sc.echoed(words, "\n", true)
	default:
		sc.unknown = true
	}
}

func (sc *scan) echoed(words []*syntax.Word, sep string, printf bool) {
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
		saved := maps.Clone(sc.vars)
		defer func() { sc.vars = saved }()
	}
	for _, a := range ce.Assigns {
		sc.assign(a)
	}
	read := func() {
		for j, w := range u.before {
			if isAssignment(w) {
				sc.export(w)
			} else if opt, ok := u.attachedChdir[j]; ok {
				sc.arg(w, folder)
				if fields, err := sc.fields(w); err == nil && len(fields) == 1 {
					val := strings.TrimPrefix(fields[0], opt)
					if strings.HasPrefix(opt, "--") {
						val = strings.TrimPrefix(val, "=")
					}
					sc.intoPath(val)
				} else {
					sc.unknown = true
				}
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
	if slices.Contains(u.roles, target) || len(u.attachedChdir) > 0 {
		sc.subshell(read)
	} else {
		read()
	}
}

func (sc *scan) export(w *syntax.Word) {
	sc.arg(w, slashedOnly)
	name, _, _ := strings.Cut(leadingLit(w), "=")
	sc.vars[name] = nil
	if fields, err := sc.fields(w); err == nil {
		_, v, _ := strings.Cut(strings.Join(fields, " "), "=")
		sc.vars[name] = &v
	}
}

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
		pushed = sc.dir != from || sc.sameDirTarget(args[0])
	case push:
		if n := len(sc.pushed); n > 0 {
			sc.dir, sc.pushed[n-1] = sc.pushed[n-1], sc.dir
		}
	default:
		if home, ok := sc.lookup("HOME"); ok {
			sc.candidate(home, target)
		}
	}
	if push && pushed {
		sc.pushed = append(sc.pushed, from)
	}
}

func (sc *scan) sameDirTarget(w *syntax.Word) bool {
	if fields, err := sc.fields(w); err == nil && len(fields) == 1 {
		p, ok := sc.path(fields[0], sc.dir)
		return ok && p == sc.dir
	}
	return false
}

func (sc *scan) cdpath(w *syntax.Word) {
	cdpath, ok := sc.lookup("CDPATH")
	if !ok || cdpath == "" {
		return
	}
	fields, err := sc.fields(w)
	if err != nil || len(fields) != 1 || filepath.IsAbs(fields[0]) || fields[0] == "." || fields[0] == ".." ||
		strings.HasPrefix(fields[0], "./") || strings.HasPrefix(fields[0], "../") {
		return
	}
	for _, d := range filepath.SplitList(cdpath) {
		if d != "" {
			sc.candidate(filepath.Join(d, fields[0]), target)
		}
	}
}

func (sc *scan) assign(a *syntax.Assign) {
	sc.substitutions(a.Index)
	if a.Array != nil {
		for _, elem := range a.Array.Elems {
			sc.substitutions(elem.Index)
			if elem.Value != nil {
				sc.arg(elem.Value, plain)
			}
		}
		if a.Name != nil {
			sc.vars[a.Name.Value] = nil
		}
		return
	}
	switch {
	case a.Value == nil:
	case a.Name == nil || a.Naked:
		sc.arg(a.Value, plain)
	default:
		sc.arg(a.Value, slashedOnly)
		prior, hadPrior := sc.lookup(a.Name.Value)
		sc.vars[a.Name.Value] = nil
		if fields, err := sc.fields(a.Value); err == nil {
			v := strings.Join(fields, " ")
			if a.Append && hadPrior {
				v = prior + v
			}
			sc.vars[a.Name.Value] = &v
		}
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
	for _, v := range values {
		if sc.vars[iter.Name.Value] = v; !sc.spend(len(fc.Do)) {
			return
		}
		sc.stmts(fc.Do, false)
	}
}

func (sc *scan) redirect(r *syntax.Redirect, shell bool) {
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		sc.heredoc(r.Hdoc, shell)
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
		if l := r.Word.Lit(); l == "-" || strings.Trim(l, "0123456789") == "" {
			return
		}
		fallthrough
	default:
		sc.arg(r.Word, creates)
	}
}

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

func (sc *scan) body(text string, shell bool) {
	if !shell {
		sc.fragments(text, data)
		return
	}
	if sc.feedBuf != nil {
		if sc.charge(len(text) + 1) {
			sc.feedBuf.WriteString(text)
			sc.feedBuf.WriteByte('\n')
		}
		return
	}
	sc.subshell(func() { sc.script(text) })
}

func (sc *scan) fragments(text string, r role) {
	for f := range fragments(text) {
		if sc.unknown {
			return
		}
		sc.candidate(f, r)
	}
}

func (sc *scan) inline(words []*syntax.Word, shell bool) {
	outerFeed := sc.feedBuf
	sc.feedBuf = nil
	defer func() { sc.feedBuf = outerFeed }()
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

func (sc *scan) substitutions(node syntax.Node) {
	if node == nil {
		return
	}
	outerFeed := sc.feedBuf
	sc.feedBuf = nil
	defer func() { sc.feedBuf = outerFeed }()
	syntax.Walk(node, func(n syntax.Node) bool {
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
