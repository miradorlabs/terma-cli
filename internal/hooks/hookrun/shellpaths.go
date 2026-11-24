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

// ShellPaths returns the folders command may reach from cwd (and UnknownReach on static
// ambiguity, budget/deadline overflow, or remote shares). It never executes commands.
func ShellPaths(command, cwd string) []string { return shellPaths(command, cwd, os.LookupEnv) }
func shellPaths(command, cwd string, env func(string) (string, bool)) []string {
	return shellPathsBy(command, cwd, env, time.Now().Add(maxShellTime))
}

const windowsShells = filepath.Separator == '\\'

func shellPathsBy(command, cwd string, env func(string) (string, bool), deadline time.Time) []string {
	return readShell(command, shellReadings(command, windowsShells), cwd, env, deadline)
}
func readShell(command string, readings []string, cwd string, env func(string) (string, bool), deadline time.Time) []string {
	return readShellWith(command, readings, cwd, env, work{deadline: deadline})
}
func readShellWith(command string, readings []string, cwd string, env func(string) (string, bool), done work) []string {
	var out []string
	unknown := false
	for _, src := range readings {
		sc := scanShell(src, cwd, env, done)
		done, unknown = sc.work, unknown || sc.unknown
		for _, p := range sc.out {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	if len(out) > maxShellPlaces {
		out, unknown = out[:maxShellPlaces], true
	}
	if unknown {
		out = append(out, unknownReach(command))
	}
	return out
}
func shellReadings(command string, windows bool) []string {
	if windows && strings.ContainsRune(command, '\\') {
		if alt := windowsReading(command); alt != command {
			return []string{command, alt}
		}
	}
	return []string{command}
}
func windowsReading(command string) string {
	parts := strings.Split(command, `'\''`)
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(p, `\`, "/")
	}
	return strings.Join(parts, `'\''`)
}
func scanShell(src, cwd string, env func(string) (string, bool), done work) (sc *scan) {
	sc = newScan(cwd, env, done)
	if !done.noRecover {
		defer func() {
			if recover() != nil {
				sc.unknown = true
			}
		}()
	}
	sc.script(src)
	return sc
}

type branchState struct {
	dir, prev string
	pushed    []string
	vars      map[string]*string
	loopVars  map[string]bool
}

func (sc *scan) snapshot() branchState {
	return branchState{sc.dir, sc.prev, slices.Clone(sc.pushed), maps.Clone(sc.vars), maps.Clone(sc.loopVars)}
}
func (sc *scan) restore(s branchState) {
	sc.dir, sc.prev, sc.pushed, sc.vars, sc.loopVars = s.dir, s.prev, slices.Clone(s.pushed), maps.Clone(s.vars), maps.Clone(s.loopVars)
}
func (sc *scan) setLoopVar(name string, on bool) {
	if !on {
		delete(sc.loopVars, name)
	} else {
		if sc.loopVars == nil {
			sc.loopVars = map[string]bool{}
		}
		sc.loopVars[name] = true
	}
}
func (sc *scan) mergeBranches(states []branchState) {
	if len(states) == 0 || sc.unknown {
		return
	}
	first, merged := states[0], maps.Clone(states[0].vars)
	for _, st := range states[1:] {
		if st.dir != first.dir || st.prev != first.prev || !slices.Equal(st.pushed, first.pushed) {
			sc.unknown = true
			return
		}
		for k, v0 := range merged {
			if v, ok := st.vars[k]; !ok || v0 == nil || v == nil || *v != *v0 {
				merged[k] = nil
				delete(sc.loopVars, k)
			}
		}
		for k := range st.vars {
			if _, ok := merged[k]; !ok {
				merged[k] = nil
				delete(sc.loopVars, k)
			}
		}
	}
	sc.dir, sc.prev, sc.pushed, sc.vars = first.dir, first.prev, slices.Clone(first.pushed), merged
}
func (sc *scan) ifClause(c *syntax.IfClause) {
	ends, hasElse := []branchState(nil), false
	for ; c != nil && !sc.unknown; c = c.Else {
		if len(c.Cond) > 0 {
			sc.stmts(c.Cond, false)
		} else {
			hasElse = true
		}
		before := sc.snapshot()
		sc.stmts(c.Then, false)
		ends = append(ends, sc.snapshot())
		sc.restore(before)
	}
	if !hasElse {
		ends = append(ends, sc.snapshot())
	}
	sc.mergeBranches(ends)
}
func (sc *scan) caseClause(c *syntax.CaseClause) {
	sc.substitutions(c.Word)
	before, hasDefault, ends := sc.snapshot(), false, []branchState(nil)
	for _, item := range c.Items {
		if sc.unknown {
			return
		}
		for _, pat := range item.Patterns {
			sc.substitutions(pat)
			hasDefault = hasDefault || pat.Lit() == "*"
		}
		sc.stmts(item.Stmts, false)
		ends = append(ends, sc.snapshot())
		if item.Op == syntax.Break || item.Op == 0 {
			sc.restore(before)
		}
	}
	if !hasDefault {
		ends = append(ends, before)
	}
	sc.mergeBranches(ends)
}
func (sc *scan) script(src string) {
	if sc.depth == maxShellDepth || !sc.charge(len(src)) || !parsable(src) {
		sc.unknown = true
		return
	}
	outer, outerFeed := sc.src, sc.feedBuf
	sc.src, sc.feedBuf, sc.depth = src, nil, sc.depth+1
	defer func() { sc.src, sc.feedBuf, sc.depth = outer, outerFeed, sc.depth-1 }()
	var stmts []*syntax.Stmt
	var err error
	for s, e := range syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).StmtsSeq(strings.NewReader(src)) {
		if err = e; err != nil {
			break
		}
		stmts = append(stmts, s)
	}
	end := 0
	for _, s := range stmts {
		syntax.Walk(s, func(n syntax.Node) bool {
			if c, ok := n.(*syntax.Comment); ok && c.Hash.Offset() > 0 && strings.IndexByte(" \t\n;&|()<>", src[c.Hash.Offset()-1]) < 0 {
				sc.unknown = true
			}
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

var moves = []string{"cd", "pushd", "popd"}

func (sc *scan) rest(text string) {
	if strings.Contains(strings.ReplaceAll(text, "$((", ""), "$(") || strings.ContainsAny(text, "`") ||
		strings.Contains(text, "<(") || strings.Contains(text, ">(") || strings.Contains(text, "=(") {
		sc.unknown = true
		return
	}
	parser, seen, unquote := syntax.NewParser(), map[string]bool{}, strings.NewReplacer(`"`, "", "'", "")
	outerSrc, outerReads := sc.src, sc.reads
	sc.reads = &operands{}
	defer func() { sc.src, sc.reads = outerSrc, outerReads }()
	for w := range strings.FieldsFuncSeq(text, func(r rune) bool { return r <= ' ' || strings.ContainsRune(";&|<>()", r) }) {
		if w = unquote.Replace(w); w == "" || seen[w] {
			continue
		}
		if seen[w] = true; slices.Contains(moves, w) || !sc.spend(1) || !sc.charge(len(w)) {
			sc.unknown = true
			return
		}
		if len(w) <= maxShellExpr {
			if word, err := parser.Document(strings.NewReader(w)); err == nil {
				sc.src = w
				sc.arg(word, plain)
				continue
			}
		}
		if strings.ContainsAny(w, "$*?[{") {
			sc.unknown = true
			return
		}
		sc.candidate(w, plain)
	}
	for _, d := range sc.reads.folders {
		sc.below(d, maxBelowDepth+2)
	}
}
