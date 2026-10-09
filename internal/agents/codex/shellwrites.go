package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// shellWrites returns the files a shell command names as written (redirect targets, the
// operands of the writers in writtenIndexes, and the files of a patch it runs), in the
// order it writes them, resolved against cwd and any cd before them; commits holds, for
// each git commit it makes, how many of them come before it. A command that does not parse
// gives only the files of a patch in its text.
func shellWrites(command, cwd string) (paths []string, commits []int) {
	paths, commits, _, _, _ = shellWritePlan(command, cwd)
	return paths, commits
}

// shellWritePlan keeps extraction separate from predictions: a named write is safe to
// expect only on every control-flow path reaching the first commit, or (after the call)
// on every path completing after its last commit.
func shellWritePlan(command, cwd string) (paths []string, commits []int, before, after, reported []string) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		for _, p := range applyPatchPaths(command) {
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			paths = append(paths, filepath.Clean(p))
		}
		return paths, nil, paths, paths, paths
	}
	dir := cwd
	var out []string
	var sources []*syntax.Stmt
	var stmt *syntax.Stmt
	boundaries := map[*syntax.Stmt]bool{}
	successful := map[*syntax.Stmt]bool{}
	directoryStates := shellDirectoryStates(file)
	created := map[*syntax.Stmt]map[string]bool{}
	possibleMade := map[string]bool{}
	resolve := func(w word) (string, bool) {
		if !w.literal || w.text == "" || (dir == "" && !filepath.IsAbs(w.text)) {
			return "", false
		}
		p := filepath.Clean(w.text)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return p, true
	}
	// made holds only directories whose mkdir must have succeeded before this statement.
	made := map[string]bool{}
	add := func(w word) {
		p, ok := resolve(w)
		if !ok || made[p] || possibleMade[p] {
			return // a directory the command makes: mv's and cp's file lands under it
		}
		if info, err := os.Stat(p); err == nil && !info.Mode().IsRegular() {
			return // a directory, or /dev/null
		} else if err != nil && !made[filepath.Dir(p)] {
			if parent, err := os.Stat(filepath.Dir(p)); err != nil || !parent.IsDir() {
				return // nothing was written there: cp's into a file, a stale cd
			}
		}
		out = append(out, p)
		sources = append(sources, stmt)
	}
	type scope struct {
		node syntax.Node
		dir  string
		stmt *syntax.Stmt
		made map[string]bool
	}
	var stack []scope
	syntax.Walk(file, func(n syntax.Node) bool {
		if n == nil {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			stmt = top.stmt
			made = top.made
			switch t := top.node.(type) {
			case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
				dir = top.dir // a cd inside ends with it
			case *syntax.BinaryCmd:
				if t.Op == syntax.Pipe || t.Op == syntax.PipeAll {
					dir = top.dir // so does one in a pipeline, each part a subshell
				}
			case *syntax.IfClause, *syntax.CaseClause, *syntax.WhileClause, *syntax.ForClause:
				if dir != top.dir {
					dir = "" // a cd in a branch or loop may not have run
				}
			}
			return true
		}
		parentStmt := stmt
		parentMade := made
		switch n := n.(type) {
		case *syntax.FuncDecl:
			return false // defining a function runs nothing
		case *syntax.Stmt:
			stmt = n
			made = map[string]bool{}
			for previous := range directoryStates[n].all {
				for path := range created[previous] {
					made[path] = true
				}
			}
			// apply_patch takes its patch as an argument or a heredoc; it makes any directory.
			for _, patch := range patches(n) {
				for _, p := range applyPatchPaths(patch) {
					if p, ok := resolve(word{p, true}); ok {
						out = append(out, p)
						sources = append(sources, stmt)
						successful[stmt] = true
					}
				}
			}
		case *syntax.Redirect:
			if written(n) {
				add(literal(n.Word))
			}
		case *syntax.CallExpr:
			words := wordsOf(n)
			if len(words) > 0 && words[0].text == "cd" {
				switch {
				case len(words) == 2 && words[1].literal && filepath.IsAbs(words[1].text):
					dir = filepath.Clean(words[1].text)
				case len(words) == 2 && words[1].literal && dir != "":
					dir = filepath.Join(dir, words[1].text)
				default:
					dir = "" // somewhere the shell decides; relative targets after it are unknown
				}
			}
			if len(words) > 0 && filepath.Base(words[0].text) == "mkdir" && directoryStates[stmt].live {
				dirs := map[string]bool{}
				args := make([]string, len(words))
				for i, w := range words {
					args[i] = w.text
				}
				for _, i := range operands(args, "-m", "--mode") {
					if p, ok := resolve(words[i]); ok {
						dirs[p] = true
						if slices.Contains(args, "-p") || slices.Contains(args, "--parents") {
							for parent := filepath.Dir(p); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
								dirs[parent] = true
							}
						}
					}
				}
				created[stmt] = dirs
				for path := range dirs {
					possibleMade[path] = true
				}
			}
			writeWords, shellDir := words, dir
			gitMove := len(words) > 0 && filepath.Base(words[0].text) == "git"
			if gitMove {
				writeWords, dir = gitCommand(words, dir)
				if len(writeWords) == 0 || writeWords[0].text != "mv" {
					writeWords = nil
				}
			}
			var moved []string
			var handled bool
			if gitMove {
				moved, handled = directoryMovePaths(writeWords, dir, made, possibleMade)
			}
			if handled {
				out = append(out, moved...)
				for range moved {
					sources = append(sources, stmt)
				}
				successful[stmt] = len(moved) > 0
			} else {
				operands := writtenOperands(writeWords)
				if len(operands) > 0 {
					successful[stmt] = true
				}
				for _, w := range operands {
					add(w)
				}
			}
			dir = shellDir // git -C affects this invocation, not the shell
			if makesCommit(words) {
				commits = append(commits, len(out))
				boundaries[stmt] = true
			}
		}
		stack = append(stack, scope{n, dir, parentStmt, parentMade})
		return true
	})
	pre, post, report := guaranteedShellWrites(file, sources, boundaries, successful)
	for i, source := range sources {
		if pre[source] {
			before = append(before, out[i])
		}
		if post[source] {
			after = append(after, out[i])
		}
		if report[source] {
			reported = append(reported, out[i])
		}
	}
	return out, commits, before, after, reported
}

// patches returns the patch text an apply_patch statement is given.
func patches(st *syntax.Stmt) []string {
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok {
		return nil
	}
	words := wordsOf(call)
	if len(words) == 0 || filepath.Base(words[0].text) != "apply_patch" {
		return nil
	}
	var out []string
	for _, w := range words[1:] {
		if w.literal {
			out = append(out, w.text)
		}
	}
	for _, r := range st.Redirs {
		if r.Op != syntax.Hdoc && r.Op != syntax.DashHdoc || r.Hdoc == nil {
			continue
		}
		var b strings.Builder
		for _, p := range r.Hdoc.Parts {
			if lit, ok := p.(*syntax.Lit); ok {
				b.WriteString(lit.Value)
			}
		}
		out = append(out, b.String())
	}
	return out
}

// written reports whether a redirect sends the command's output to its word: not stderr
// alone, not a heredoc, and not a descriptor copy such as 2>&1 or >&-.
func written(r *syntax.Redirect) bool {
	if r.N != nil && r.N.Value != "1" {
		return false
	}
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll:
		return true
	case syntax.DplOut: // >& file is &> file
		w := literal(r.Word)
		return w.literal && w.text != "-" && strings.Trim(w.text, "0123456789") != ""
	}
	return false
}

type word struct {
	text string
	// literal is false when the shell would expand the word, so its text is not the path.
	literal bool
}

// literal is the word's text when the shell expands nothing in it: plain, quoted, or both.
func literal(w *syntax.Word) word {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[{\\") || strings.HasPrefix(p.Value, "~") {
				return word{}
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				l, ok := inner.(*syntax.Lit)
				if !ok {
					return word{}
				}
				b.WriteString(l.Value)
			}
		default:
			return word{}
		}
	}
	return word{b.String(), true}
}

// writtenOperands returns the arguments a command writes to.
func writtenOperands(words []word) []word {
	args := make([]string, len(words))
	for i, w := range words {
		args[i] = w.text
		if !w.literal {
			args[i] = "$" // an operand whose text is unknown, never sed's empty -i suffix
		}
	}
	var out []word
	for _, i := range writtenIndexes(args) {
		out = append(out, words[i])
	}
	if len(out) > 0 && (filepath.Base(args[0]) == "cp" || filepath.Base(args[0]) == "mv") {
		// Into a directory, a file lands under its own name; add keeps whichever path is real.
		dest := out[len(out)-1]
		for _, i := range operands(args) {
			if src := words[i]; src != dest {
				out = append(out, word{filepath.Join(dest.text, filepath.Base(src.text)), dest.literal && src.literal})
			}
		}
	}
	return out
}

// writtenIndexes returns the indexes of the arguments a command writes to.
func writtenIndexes(args []string) []int {
	if len(args) == 0 {
		return nil
	}
	ops := operands(args)
	has := func(flags ...string) bool {
		return slices.ContainsFunc(args[1:], func(a string) bool { return slices.Contains(flags, a) })
	}
	switch filepath.Base(args[0]) {
	case "tee":
		return ops
	case "sed", "gsed":
		return inPlaceOperands(args, "ef")
	case "perl":
		return inPlaceOperands(args, "eE")
	case "gofmt", "goimports":
		if has("-w") {
			return ops
		}
	case "prettier":
		if has("--write", "-w") {
			return operands(args, "--config", "--ignore-path", "--plugin", "--parser", "--log-level",
				"--cache-location", "--cache-strategy", "--stdin-filepath")
		}
	case "biome":
		ops := operands(args, "--config-path", "--stdin-file-path", "--vcs-root", "--log-level",
			"--max-diagnostics", "--diagnostic-level", "--reporter", "--log-kind")
		if has("--write", "--fix", "--apply") && len(ops) > 0 {
			return ops[1:] // after the subcommand
		}
	case "cp":
		if len(ops) >= 2 {
			return ops[len(ops)-1:]
		}
	case "mv":
		if len(ops) >= 2 {
			return ops
		}
	}
	return nil
}

// operands are the indexes of the arguments after the command name that are neither flags
// nor the value of one of valueFlags (--config .prettierrc is read, not written).
func operands(args []string, valueFlags ...string) []int {
	var out []int
	for i := 1; i < len(args); i++ {
		switch {
		case slices.Contains(valueFlags, args[i]):
			i++
		case !strings.HasPrefix(args[i], "-"):
			out = append(out, i)
		}
	}
	return out
}

// inPlaceOperands returns the files sed -i or perl -i rewrites: every operand when a script
// flag (scriptFlags, short) gave the script, else all but the first, which is the script.
func inPlaceOperands(args []string, scriptFlags string) []int {
	inPlace, script := false, false
	var ops []int
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--in-place"):
			inPlace = true
		case a == "--expression" || a == "--file":
			script = true
			i++
		case strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file="):
			script = true
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			flags := a[1:]
			// Only switches that take no argument may come before i (-pi, -Ei): in -Mstrict the
			// i belongs to -M's.
			if before, _, ok := strings.Cut(flags, "i"); ok && strings.Trim(before, noArgSwitches) == "" {
				inPlace, flags = true, before // what follows -i is its backup suffix
				if a == "-i" && i+1 < len(args) && args[i+1] == "" {
					i++ // BSD sed's empty suffix: sed -i '' ...
				}
			}
			if j := strings.IndexAny(flags, scriptFlags); j >= 0 && strings.Trim(flags[:j], noArgSwitches) == "" {
				script = true
				if j == len(flags)-1 {
					i++ // the script is the next argument, not attached as in -e's/a/b/
				}
			}
		default:
			ops = append(ops, i)
		}
	}
	if !inPlace {
		return nil
	}
	if !script && len(ops) > 0 {
		ops = ops[1:]
	}
	return ops
}

// noArgSwitches are sed and perl switches that take no argument, so others may follow them in
// one cluster.
const noArgSwitches = "nprslaEz"
