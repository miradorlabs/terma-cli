package hookrun

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

var windowsReplacer = strings.NewReplacer(`'\''`, `'\''`, `\;`, `\;`, `\`, "/")

type branchState struct {
	dir, prev    string
	dirUncertain bool
	pushed       []string
	vars         map[string]*string
	funcs        map[string]*syntax.Stmt
	loopVars     map[string]bool
}

// ShellPaths returns the folders command may reach from cwd (and UnknownReach on static
// ambiguity, budget/deadline overflow, or remote shares). It never executes commands.
func ShellPaths(command, cwd string) []string {
	return shellPathsBy(command, cwd, os.LookupEnv, time.Now().Add(maxShellTime))
}
func shellPathsBy(cmd, cwd string, env func(string) (string, bool), d time.Time) []string {
	return readShellWith(cmd, shellReadings(cmd, filepath.Separator == '\\'), cwd, env, work{deadline: d})
}
func readShellWith(command string, readings []string, cwd string, env func(string) (string, bool), done work) []string {
	out, unknown := []string(nil), false
	for _, src := range readings {
		sc := scanShell(src, cwd, env, done)
		done, unknown, out = sc.work, unknown || sc.unknown, appendUnique(out, sc.out...)
	}
	return pick(unknown || len(out) > maxShellPlaces, append(out[:min(len(out), maxShellPlaces)], unknownReach(command)), out)
}
func shellReadings(command string, windows bool) []string {
	alt := windowsReplacer.Replace(command)
	return pick(windows && strings.ContainsRune(command, '\\') && alt != command, []string{command, alt}, []string{command})
}
func scanShell(src, cwd string, env func(string) (string, bool), done work) (sc *scan) {
	sc = newScan(cwd, env, done)
	defer func() { sc.unknown = sc.unknown || !done.noRecover && recover() != nil }()
	sc.script(src)
	sc.script(sc.traps)
	return sc
}
func (sc *scan) snapshot() branchState {
	return branchState{sc.dir, sc.prev, sc.dirUncertain, slices.Clone(sc.pushed), maps.Clone(sc.vars), maps.Clone(sc.funcs), maps.Clone(sc.loopVars)}
}
func (sc *scan) restore(s branchState) {
	sc.dir, sc.prev, sc.dirUncertain, sc.pushed, sc.vars, sc.funcs, sc.loopVars = s.dir, s.prev, s.dirUncertain, slices.Clone(s.pushed), maps.Clone(s.vars), maps.Clone(s.funcs), maps.Clone(s.loopVars)
}
func (sc *scan) setLoopVar(name string, on bool) { sc.loopVars[name] = on }
func (sc *scan) mergeBranches(states []branchState) {
	if len(states) == 0 || sc.unknown {
		return
	}
	first, merged, funcs, uncertain := states[0], maps.Clone(states[0].vars), maps.Clone(states[0].funcs), states[0].dirUncertain
	for _, st := range states[1:] {
		uncertain = uncertain || st.dirUncertain || st.dir != first.dir || st.prev != first.prev || !slices.Equal(st.pushed, first.pushed)
		for _, m := range []map[string]*string{merged, st.vars} {
			for k := range m {
				v0, ok0 := merged[k]
				v, ok := st.vars[k]
				if !ok0 || !ok || v0 == nil || v == nil || (v0 == unsetVar) != (v == unsetVar) || v0 != unsetVar && *v != *v0 {
					merged[k] = nil
					delete(sc.loopVars, k)
				}
			}
		}
		for _, m := range []map[string]*syntax.Stmt{funcs, st.funcs} {
			for k := range m {
				if f0, ok0 := funcs[k]; !ok0 || st.funcs[k] != f0 {
					funcs[k] = nil
				}
			}
		}
	}
	sc.dir, sc.prev, sc.dirUncertain, sc.pushed, sc.vars, sc.funcs = first.dir, first.prev, uncertain, slices.Clone(first.pushed), merged, funcs
}
func (sc *scan) ifClause(c *syntax.IfClause) {
	ends, hasElse := []branchState(nil), false
	for ; c != nil && !sc.unknown; c = c.Else {
		sc.stmts(c.Cond, false)
		hasElse = hasElse || len(c.Cond) == 0
		before := sc.snapshot()
		sc.stmts(c.Then, false)
		ends = append(ends, sc.snapshot())
		sc.restore(before)
	}
	sc.mergeBranches(pick(hasElse, ends, append(ends, sc.snapshot())))
}
func (sc *scan) caseClause(c *syntax.CaseClause) {
	sc.substitutions(c.Word)
	before, hasDefault, ends := sc.snapshot(), false, []branchState(nil)
	for i := 0; i < len(c.Items) && !sc.unknown; i++ {
		for _, pat := range c.Items[i].Patterns {
			sc.substitutions(pat)
			hasDefault = hasDefault || pat.Lit() == "*"
		}
		sc.stmts(c.Items[i].Stmts, false)
		ends = append(ends, sc.snapshot())
		if c.Items[i].Op == syntax.Break || c.Items[i].Op == 0 {
			sc.restore(before)
		}
	}
	sc.mergeBranches(pick(hasDefault, ends, append(ends, before)))
}
func (sc *scan) taintArith(node syntax.Node) {
	if node != nil {
		syntax.Walk(node, func(n syntax.Node) bool {
			var lhs syntax.ArithmExpr
			if x, ok := n.(*syntax.BinaryArithm); ok && strings.HasSuffix(x.Op.String(), "=") && !slices.Contains([]string{"==", "!=", "<=", ">="}, x.Op.String()) {
				lhs = x.X
			} else if x, ok := n.(*syntax.UnaryArithm); ok && (x.Op == syntax.Inc || x.Op == syntax.Dec) {
				lhs = x.X
			}
			if w, ok := lhs.(*syntax.Word); ok && leadingLit(w) != "" {
				sc.invalidateName(leadingLit(w))
			}
			return true
		})
	}
}
func (sc *scan) script(src string) {
	if (sc.depth == maxShellDepth || !sc.charge(len(src)) || !parsable(src)) && sc.fail() {
		return
	}
	defer func(src string, feed *strings.Builder, depth int) { sc.src, sc.feedBuf, sc.depth = src, feed, depth }(sc.src, sc.feedBuf, sc.depth)
	sc.src, sc.feedBuf, sc.depth = src, nil, sc.depth+1
	stmts, err, end := []*syntax.Stmt(nil), error(nil), 0
	for s, e := range syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).StmtsSeq(strings.NewReader(src)) {
		if err = e; err != nil {
			break
		}
		stmts = append(stmts, s)
		syntax.Walk(s, func(n syntax.Node) bool {
			c, ok := n.(*syntax.Comment)
			sc.unknown = sc.unknown || ok && c.Hash.Offset() > 0 && strings.IndexByte(" \t\n;&|()<>", src[c.Hash.Offset()-1]) < 0
			if n != nil {
				end = max(end, int(n.End().Offset()))
			}
			return true
		})
	}
	if sc.stmts(stmts, false); err != nil && !sc.unknown {
		sc.rest(src[min(end, len(src)):])
	}
}
func (sc *scan) rest(text string) {
	if (strings.Contains(strings.ReplaceAll(text, "$((", ""), "$(") || strings.ContainsAny(text, "`") || strings.Contains(text, "<(") || strings.Contains(text, ">(") || strings.Contains(text, "=(")) && sc.fail() {
		return
	}
	parser, seen, unquote := syntax.NewParser(), map[string]bool{}, strings.NewReplacer(`"`, "", "'", "")
	defer func(src string, reads *cached) { sc.src, sc.reads = src, reads }(sc.src, sc.reads)
	sc.reads = &cached{}
	for w := range strings.FieldsFuncSeq(text, func(r rune) bool { return r <= ' ' || strings.ContainsRune(";&|<>()", r) }) {
		if w = unquote.Replace(w); w == "" || seen[w] {
			continue
		} else if seen[w] = true; (w == "cd" || w == "pushd" || w == "popd" || !sc.spend(1) || !sc.charge(len(w))) && sc.fail() {
			return
		} else if len(w) <= maxShellExpr {
			if word, err := parser.Document(strings.NewReader(w)); err == nil {
				sc.src = w
				sc.arg(word, plain)
				continue
			}
		}
		if strings.ContainsAny(w, "$*?[{") && sc.fail() {
			return
		}
		sc.candidate(w, plain)
	}
	for _, d := range sc.reads.folders {
		sc.below(d, maxBelowDepth+2)
	}
}
func runsShell(cmd syntax.Command) bool {
	if ce, ok := cmd.(*syntax.CallExpr); ok && len(ce.Args) > 0 {
		if u := unwrap(ce.Args); len(u.command) > 0 {
			if s, ok := scriptOf(programName(u.command[0])); ok && s.shell {
				for i, args := 0, u.command[1:]; i < len(args); i++ {
					switch opt := args[i].Lit(); {
					case opt == "-s":
						return true
					case opt == "-o" || opt == "+o" || opt == "-O" || opt == "+O":
						i++
					case s.takes(opt), !strings.HasPrefix(opt, "-") && !strings.HasPrefix(opt, "+"):
						return false
					}
				}
				return true
			}
		}
	}
	return false
}
func runsXargs(cmd syntax.Command) bool {
	if ce, ok := cmd.(*syntax.CallExpr); ok && len(ce.Args) > 0 {
		u := unwrap(ce.Args)
		return u.xargs || len(u.command) > 0 && programName(u.command[0]) == "xargs"
	}
	return false
}

// xargsSafeFeed is whether s only lists names, resolving each link a consumer may follow: ls prints bare names, others paths.
func (sc *scan) xargsSafeFeed(s *syntax.Stmt) bool {
	ce, ok := s.Cmd.(*syntax.CallExpr)
	if u := unwrap(pick(ok, ce, &syntax.CallExpr{}).Args); ok && len(u.command) > 0 && !u.unknown {
		prog, rest, roots := programName(u.command[0]), u.command[1:], []string{sc.dir}
		ls, lister := prog == "ls", prog == "ls" || prog == "find" || prog == "fd" || prog == "git" && len(rest) > 0 && rest[0].Lit() == "ls-files"
		for _, w := range pick(lister, rest, nil) { // no format, link target, followed link or (ls) . and ..
			fields, err := sc.fields(w)
			roots, lister = append(roots, fields...), lister && err == nil && !slices.ContainsFunc(fields, func(f string) bool {
				return isFindExec(prog, f) || slices.Contains([]string{"-printf", "-fprintf", "-ls", "-fls", "-L", "-follow", "--follow", "--list-details"}, f) || strings.HasPrefix(f, "--format") ||
					prog == "git" && strings.HasPrefix(f, ":") || prog == "fd" && isShortFlag(f) && strings.ContainsAny(f, "lL") || ls && strings.HasPrefix(f, "-") && strings.Trim(f[1:], "1AdrStUX") != ""
			})
		}
		for _, r := range pick(lister, roots, nil) {
			sc.links(r, pick(ls, -1, maxBelowDepth))
		}
		return lister
	}
	return false
}
func (sc *scan) links(dir string, depth int) {
	for _, e := range sc.readDir(dir) {
		if p := filepath.Join(dir, e.Name()); e.Type()&os.ModeSymlink != 0 || strings.ContainsAny(e.Name(), " \t\n'\"\\") && sc.fail() {
			sc.candidate(p, plain) // a name xargs splits or unquotes fails closed
		} else if e.IsDir() && depth >= 0 && !sc.unknown && (depth > 0 || !sc.fail()) { // fail closed past depth, none below -1
			sc.links(p, depth-1)
		}
	}
}
