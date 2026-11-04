package main

import (
	"fmt"
	"strings"
)

// Text diffs: a unified diff of two texts, line by line, with three lines of context, the
// way a person reads a system prompt's change. A paragraph on one long line is compared
// sentence by sentence, or a word changed in it would read as the whole paragraph changed.

const (
	diffContext = 3
	longLine    = 300
	maxLCS      = 25_000_000
)

// diffLines are a text's lines for comparing: long ones split after each sentence.
func diffLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if len(line) <= longLine {
			out = append(out, line)
			continue
		}
		for len(line) > 0 {
			i := strings.Index(line, ". ")
			if i < 0 {
				out = append(out, line)
				break
			}
			out = append(out, line[:i+1])
			line = line[i+2:]
		}
	}
	return out
}

type edit struct {
	op   byte // ' ', '-', '+'
	text string
}

// edits are the steps from a to b by their longest common subsequence.
func edits(a, b []string) []edit {
	// Trim the common head and tail first: prompts change in places.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(ma), len(mb)
	if n*m > maxLCS {
		// Too large to compare line by line: the middle reads as replaced.
		out := make([]edit, 0, len(a)+len(b))
		for _, l := range a[:pre] {
			out = append(out, edit{' ', l})
		}
		for _, l := range ma {
			out = append(out, edit{'-', l})
		}
		for _, l := range mb {
			out = append(out, edit{'+', l})
		}
		for _, l := range a[len(a)-suf:] {
			out = append(out, edit{' ', l})
		}
		return out
	}
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if ma[i] == mb[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	out := make([]edit, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		out = append(out, edit{' ', l})
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case ma[i] == mb[j]:
			out = append(out, edit{' ', ma[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, edit{'-', ma[i]})
			i++
		default:
			out = append(out, edit{'+', mb[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, edit{'-', ma[i]})
	}
	for ; j < m; j++ {
		out = append(out, edit{'+', mb[j]})
	}
	for _, l := range a[len(a)-suf:] {
		out = append(out, edit{' ', l})
	}
	return out
}

// unifiedDiff is the unified diff from a to b, and the characters it adds and removes; ""
// where they are the same.
func unifiedDiff(a, b string) (string, TextStats) {
	es := edits(diffLines(a), diffLines(b))
	var stats TextStats
	for _, e := range es {
		switch e.op {
		case '-':
			stats.RemovedChars += len(e.text)
		case '+':
			stats.AddedChars += len(e.text)
		}
	}
	if stats.AddedChars == 0 && stats.RemovedChars == 0 && a == b {
		return "", stats
	}
	var sb strings.Builder
	// Hunks: runs of changes with their context, joined where the context would overlap.
	for start := 0; start < len(es); {
		for start < len(es) && es[start].op == ' ' {
			start++
		}
		if start >= len(es) {
			break
		}
		lo := max(0, start-diffContext)
		end := start
		for {
			for end < len(es) && es[end].op != ' ' {
				end++
			}
			next := end
			for next < len(es) && es[next].op == ' ' {
				next++
			}
			if next < len(es) && next-end <= 2*diffContext {
				end = next
				continue
			}
			break
		}
		hi := min(len(es), end+diffContext)
		aStart, bStart := 1, 1
		for _, e := range es[:lo] {
			if e.op != '+' {
				aStart++
			}
			if e.op != '-' {
				bStart++
			}
		}
		aLen, bLen := 0, 0
		for _, e := range es[lo:hi] {
			if e.op != '+' {
				aLen++
			}
			if e.op != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart, aLen, bStart, bLen)
		for _, e := range es[lo:hi] {
			sb.WriteByte(e.op)
			sb.WriteString(e.text)
			sb.WriteByte('\n')
		}
		start = hi
	}
	return sb.String(), stats
}
