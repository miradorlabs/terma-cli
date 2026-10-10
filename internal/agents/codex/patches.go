package codex

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// pipeline is the statement the innermost statement on stack runs in: the whole pipeline it
// is part of, whose other commands may feed it a patch.
func pipeline(stack []scope) syntax.Node {
	i := len(stack) - 1
	for i >= 2 {
		b, ok := stack[i-1].node.(*syntax.BinaryCmd)
		if !ok || b.Op != syntax.Pipe && b.Op != syntax.PipeAll {
			break
		}
		i -= 2
	}
	return stack[i].node
}

// textOf is the literal text in n, a piece per line: enough to find a patch's headers.
func textOf(n syntax.Node) string {
	var b strings.Builder
	syntax.Walk(n, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Lit:
			b.WriteString(n.Value + "\n")
		case *syntax.SglQuoted:
			b.WriteString(n.Value + "\n")
		}
		return true
	})
	return b.String()
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
