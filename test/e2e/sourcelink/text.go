package sourcelink

import (
	"cmp"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Finding prose (FindText). A build's prompt is assembled from its source's text: a Markdown
// file, a template literal, a run of concatenated strings, each with what the build fills in
// (a path, a date, a tool's name) between. So the text of every line, a prose file's whole
// line or the part of a string a line of code holds, is cut at what is filled in, and each
// fragment long enough to be distinctive is indexed by its first words; a passage asked
// about is matched against the fragments its words start, and the lines that hold the most
// of it, close together in one file, are where it lives.

const (
	// keyWords is how many words a fragment is indexed by, and the fewest it may have.
	keyWords = 4
	// minFragment is the shortest fragment kept: a line shorter says too little to place a
	// passage.
	minFragment = 20
	// maxFragmentHits leaves out a fragment the tree holds in more places: boilerplate (a
	// license header) that places nothing.
	maxFragmentHits = 25
	// runGap is how far apart two lines of a file may be and still hold one passage: a
	// paragraph break, a line of code between two strings.
	runGap = 4
	// minCover is how much of a passage a run must hold to be said to hold it, or most of a
	// passage shorter than twice this.
	minCover = 60
	// maxTextRefs bounds the runs a passage is linked to.
	maxTextRefs = 10
)

// placeholder is what a build fills into its text: ${expr}, {name}, {{name}}, %(name)s, %s.
var placeholder = regexp.MustCompile(`\$\{[^{}]*\}|\{\{[^{}]*\}\}|\{[A-Za-z_][\w.\[\]'"]*(?:![rsa])?(?::[^{}]*)?\}|%\(\w+\)[sdrf]|%[sd]`)

// fragments are the distinctive parts of a line's text, whitespace collapsed.
func fragments(text string) []string {
	var out []string
	for _, part := range placeholder.Split(text, -1) {
		words := strings.Fields(part)
		// What is left of a sentence after what was filled in ("…today is %s: be careful")
		// starts at its next word, as the passage's own words do.
		for len(words) > 0 && !strings.ContainsFunc(words[0], func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			words = words[1:]
		}
		if len(words) < keyWords {
			continue
		}
		if f := strings.Join(words, " "); len(f) >= minFragment {
			out = append(out, f)
		}
	}
	return out
}

// fragment is a piece of a tree's text, and the lines that hold it.
type fragment struct {
	text string
	hits []Hit
}

// textIndex is a tree's text, its fragments by their first words.
type textIndex struct {
	byKey map[string][]*fragment
}

// keyOf is a fragment's first keyWords words.
func keyOf(f string) string {
	end, words := 0, 0
	for end < len(f) {
		sp := strings.IndexByte(f[end:], ' ')
		if sp < 0 {
			end = len(f)
		} else {
			end += sp
		}
		if words++; words == keyWords {
			break
		}
		end++
	}
	return f[:min(end, len(f))]
}

// indexText reads the text of a tree of s: its prose files whole, and its code's strings,
// outside tests.
func indexText(r io.Reader, s Source, lang *language) (*textIndex, error) {
	x := &textIndex{byKey: map[string][]*fragment{}}
	seen := map[string]*fragment{}
	add := func(text string, h Hit) {
		for _, f := range fragments(text) {
			fr := seen[f]
			if fr == nil {
				fr = &fragment{text: f}
				seen[f] = fr
				x.byKey[keyOf(f)] = append(x.byKey[keyOf(f)], fr)
			}
			if len(fr.hits) <= maxFragmentHits {
				fr.hits = append(fr.hits, h)
			}
		}
	}
	prose := func(file string) bool {
		return slices.ContainsFunc(proseExts, func(ext string) bool { return strings.HasSuffix(strings.ToLower(file), ext) })
	}
	want := func(file string) bool { return (lang.code(file) || prose(file)) && !lang.testFile(file) }
	err := tarFiles(r, s, want, func(file string, r io.Reader) error {
		if prose(file) {
			return lines(file, r, func(n int, line string) { add(line, Hit{file, n, 0}) })
		}
		lex := newLexer(s.Lang, true)
		return lines(file, r, func(n int, line string) {
			_, text := lex.line(line)
			for _, t := range text {
				add(t, Hit{file, n, 0})
			}
		})
	})
	if err != nil {
		return nil, err
	}
	return x, nil
}

// find is where a tree holds a passage: the runs of lines that hold the most of it.
func (x *textIndex) find(s Source, version, text string) []Ref {
	p := strings.Join(strings.Fields(text), " ")
	var starts []int
	for i := 0; i < len(p); i++ {
		if i == 0 || p[i-1] == ' ' {
			starts = append(starts, i)
		}
	}
	type match struct {
		hit        Hit
		start, end int // the part of the passage the line holds
	}
	var matches []match
	for i := 0; i+keyWords <= len(starts); i++ {
		key := keyOf(p[starts[i]:])
		for _, f := range x.byKey[key] {
			if len(f.hits) > maxFragmentHits || !strings.HasPrefix(p[starts[i]:], f.text) {
				continue
			}
			for _, h := range f.hits {
				matches = append(matches, match{h, starts[i], starts[i] + len(f.text)})
			}
		}
	}
	slices.SortFunc(matches, func(a, b match) int {
		return cmp.Or(strings.Compare(a.hit.Path, b.hit.Path), a.hit.Line-b.hit.Line, a.start-b.start)
	})
	// A run is a file's matching lines, each within runGap of the last; it holds the part of
	// the passage its lines' fragments cover together.
	type run struct {
		path        string
		first, last int
		cover       int
	}
	var runs []run
	for i := 0; i < len(matches); {
		j := i + 1
		for j < len(matches) && matches[j].hit.Path == matches[i].hit.Path && matches[j].hit.Line-matches[j-1].hit.Line <= runGap {
			j++
		}
		spans := matches[i:j]
		slices.SortFunc(spans, func(a, b match) int { return a.start - b.start })
		cover, reach := 0, 0
		for _, m := range spans {
			if m.end > reach {
				cover += m.end - max(m.start, reach)
				reach = m.end
			}
		}
		first, last := spans[0].hit.Line, spans[0].hit.Line
		for _, m := range spans {
			first, last = min(first, m.hit.Line), max(last, m.hit.Line)
		}
		runs = append(runs, run{spans[0].hit.Path, first, last, cover})
		i = j
	}
	need := min(minCover, len(p)/2)
	slices.SortStableFunc(runs, func(a, b run) int {
		return cmp.Or(b.cover-a.cover, strings.Compare(a.path, b.path), a.first-b.first)
	})
	var out []Ref
	for _, r := range runs {
		if r.cover < need || r.cover < minFragment || len(out) == maxTextRefs {
			break
		}
		url := s.Blob(version, r.path, r.first)
		if r.last > r.first {
			url += fmt.Sprintf("-L%d", r.last)
		}
		out = append(out, Ref{Path: r.path, Line: r.first, URL: url})
	}
	return out
}
