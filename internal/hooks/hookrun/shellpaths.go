package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

// ShellPaths returns the folders command may reach from cwd (and UnknownReach on static
// ambiguity, budget/deadline overflow, or remote shares). It never executes commands.
func ShellPaths(command, cwd string) []string {
	return shellPaths(command, cwd, os.LookupEnv)
}

func shellPaths(command, cwd string, env func(string) (string, bool)) []string {
	return shellPathsBy(command, cwd, env, time.Now().Add(maxShellTime))
}

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

const windowsShells = filepath.Separator == '\\'

func shellReadings(command string, windows bool) []string {
	if windows && strings.ContainsRune(command, '\\') {
		return []string{command, windowsReading(command)}
	}
	return []string{command}
}

func windowsReading(command string) string { return strings.ReplaceAll(command, `\`, "/") }

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
	sc.stmts(stmts, false)
	if err != nil && !sc.unknown {
		sc.rest(src[min(end, len(src)):])
	}
}

var moves = []string{"cd", "pushd", "popd"}

func (sc *scan) rest(text string) {
	stripped := strings.ReplaceAll(text, "$((", "")
	if strings.Contains(stripped, "$(") || strings.ContainsRune(text, '`') ||
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
