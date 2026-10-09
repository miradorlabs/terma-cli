package codex

import (
	"maps"
	"path/filepath"

	"mvdan.cc/sh/v3/syntax"
)

// A state contains writes reached on every route represented by it. Keeping routes
// with and without a commit apart prevents later writes from claiming an earlier one.
type shellFlowState struct {
	live   bool
	writes map[*syntax.Stmt]bool
	all    map[*syntax.Stmt]bool
}

type shellFlowRoutes [2]shellFlowState  // no commit yet, at least one commit
type shellFlowResult [2]shellFlowRoutes // failure, success

func mergeShellState(a, b shellFlowState) shellFlowState {
	if !a.live {
		return b
	}
	if !b.live {
		return a
	}
	common := maps.Clone(a.writes)
	for stmt := range common {
		if !b.writes[stmt] {
			delete(common, stmt)
		}
	}
	all := maps.Clone(a.all)
	for stmt := range all {
		if !b.all[stmt] {
			delete(all, stmt)
		}
	}
	return shellFlowState{live: true, writes: common, all: all}
}

func mergeShellRoutes(a, b shellFlowRoutes) shellFlowRoutes {
	return shellFlowRoutes{mergeShellState(a[0], b[0]), mergeShellState(a[1], b[1])}
}

type shellFlow struct {
	writers, commits map[*syntax.Stmt]bool
	requiresSuccess  map[*syntax.Stmt]bool
	entries          map[*syntax.Stmt]shellFlowState
	first            shellFlowState
}

func guaranteedShellWrites(file *syntax.File, sources []*syntax.Stmt, commits, successful map[*syntax.Stmt]bool) (before, after, reported map[*syntax.Stmt]bool) {
	f := shellFlow{writers: map[*syntax.Stmt]bool{}, commits: commits, requiresSuccess: successful}
	for _, stmt := range sources {
		f.writers[stmt] = true
	}
	end := f.list(file.Stmts, shellFlowRoutes{{live: true}})
	// PostToolUse describes a completed call. An unsuccessful call can still leave a
	// prediction behind, just as a writer can fail after its pre-hook has run.
	final := end[1]
	remaining := mergeShellState(final[0], final[1])
	if f.first.live {
		before = f.first.writes
	} else {
		before = remaining.writes
	}
	return before, remaining.writes, remaining.all
}

// shellDirectoryStates names the mkdir calls that must have succeeded before each
// statement. Possible branch/loop effects are not treated as completed effects.
func shellDirectoryStates(file *syntax.File) map[*syntax.Stmt]shellFlowState {
	dirs := map[*syntax.Stmt]bool{}
	syntax.Walk(file, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.FuncDecl); ok {
			return false
		}
		if stmt, ok := node.(*syntax.Stmt); ok {
			if call, ok := stmt.Cmd.(*syntax.CallExpr); ok {
				if words := wordsOf(call); len(words) > 0 && filepath.Base(words[0].text) == "mkdir" {
					dirs[stmt] = true
				}
			}
		}
		return true
	})
	f := shellFlow{writers: dirs, requiresSuccess: dirs, entries: map[*syntax.Stmt]shellFlowState{}}
	f.list(file.Stmts, shellFlowRoutes{{live: true}})
	return f.entries
}

func (f *shellFlow) list(stmts []*syntax.Stmt, in shellFlowRoutes) shellFlowResult {
	out := shellFlowResult{nilRoutes(), in}
	for _, stmt := range stmts {
		out = f.statement(stmt, mergeShellRoutes(out[0], out[1]))
	}
	return out
}

func nilRoutes() shellFlowRoutes { return shellFlowRoutes{} }

func (f *shellFlow) statement(stmt *syntax.Stmt, in shellFlowRoutes) shellFlowResult {
	if f.entries != nil {
		f.entries[stmt] = mergeShellState(f.entries[stmt], mergeShellState(in[0], in[1]))
	}
	if stmt.Background || stmt.Coprocess {
		return shellFlowResult{in, in} // completion does not wait for this work
	}
	if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && f.containsCommit(call) {
		return f.opaque(call, in) // substitutions run before the outer call's writes
	}
	prior := in
	for i, state := range in {
		if state.live && f.writers[stmt] {
			state.writes = maps.Clone(state.writes)
			if state.writes == nil {
				state.writes = map[*syntax.Stmt]bool{}
			}
			state.writes[stmt] = true
			state.all = maps.Clone(state.all)
			if state.all == nil {
				state.all = map[*syntax.Stmt]bool{}
			}
			state.all[stmt] = true
			in[i] = state
		}
	}
	var out shellFlowResult
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		left := f.statement(cmd.X, in)
		switch cmd.Op {
		case syntax.AndStmt:
			right := f.statement(cmd.Y, left[1])
			out = shellFlowResult{mergeShellRoutes(left[0], right[0]), right[1]}
		case syntax.OrStmt:
			right := f.statement(cmd.Y, left[0])
			out = shellFlowResult{right[0], mergeShellRoutes(left[1], right[1])}
		default:
			if f.containsCommit(cmd) {
				out = f.opaque(cmd, in) // no ordering between concurrent commits and writes
			} else {
				// Both sides start; a failed producer does not skip its consumer (tee).
				out = f.statement(cmd.Y, mergeShellRoutes(left[0], left[1]))
			}
		}
	case *syntax.Block:
		out = f.list(cmd.Stmts, in)
	case *syntax.Subshell:
		out = f.list(cmd.Stmts, in)
	case *syntax.CallExpr:
		if f.commits[stmt] {
			f.first = mergeShellState(f.first, in[0])
			past := mergeShellState(in[0], in[1])
			past.writes = nil
			in = shellFlowRoutes{{}, past}
		}
		out = shellFlowResult{in, in}
		if f.requiresSuccess[stmt] {
			out[0] = prior // an external writer's effect cannot be assumed after failure
		}
		words := wordsOf(cmd)
		if len(words) == 1 {
			switch words[0].text {
			case "true", ":":
				out[0] = nilRoutes()
			case "false":
				out[1] = nilRoutes()
			}
		}
	case *syntax.FuncDecl:
		out[1] = in // a definition executes no body
	default:
		out = f.opaque(stmt.Cmd, in)
	}
	if stmt.Negated {
		out[0], out[1] = out[1], out[0]
	}
	return out
}

// Branches, loops and substitutions are deliberately not interpreted. Their writes
// may be skipped; a possible commit still prevents later writes from being preclaimed.
func (f *shellFlow) opaque(node syntax.Node, in shellFlowRoutes) shellFlowResult {
	if f.entries != nil && node != nil {
		syntax.Walk(node, func(n syntax.Node) bool {
			if _, ok := n.(*syntax.FuncDecl); ok {
				return false
			}
			if stmt, ok := n.(*syntax.Stmt); ok {
				f.entries[stmt] = mergeShellState(f.entries[stmt], mergeShellState(in[0], in[1]))
			}
			return true
		})
	}
	if f.containsCommit(node) {
		f.first = mergeShellState(f.first, in[0])
		past := mergeShellState(in[0], in[1])
		past.writes = nil
		in[1] = mergeShellState(in[1], past)
	}
	return shellFlowResult{in, in}
}

func (f *shellFlow) containsCommit(node syntax.Node) bool {
	boundary := false
	if node != nil {
		syntax.Walk(node, func(n syntax.Node) bool {
			if _, ok := n.(*syntax.FuncDecl); ok {
				return false
			}
			if stmt, ok := n.(*syntax.Stmt); ok && f.commits[stmt] {
				boundary = true
			}
			return true
		})
	}
	return boundary
}
