package sourcelink

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// Needle is a name to find: as a quoted string, or, a field, as a name assigned to, the way a
// tracing macro names a field ("codex.turn.phase = ...") or an object literal its key.
type Needle struct {
	Text  string
	Field bool
}

// Hit is a line that names a needle, and the line of the function it is in, 0 at a file's top
// level.
type Hit struct {
	Path string
	Line int
	Fn   int
}

// MaxHits bounds the lines a scan keeps of one name: far past any a caller ranks, so it only
// bounds a pathological tree.
const MaxHits = 10000

// maxFile bounds the files read: one larger is generated, a bundle or a fixture.
const maxFile = 4 << 20

// index is every name a build's source quotes or assigns, read once for every needle a
// process asks about.
type index struct {
	strs   map[string][]Hit // the lines that quote each string
	fields map[string][]Hit // the lines that assign each name
	consts []constant       // the constants that name a string, in the tree's order
	uses   map[string][]use // the lines that use each constant's name
	n      int              // uses so far, their order across names
}

// constant is a name that holds a string.
type constant struct{ name, lit string }

// use is a line that names a constant, n its order in the tree.
type use struct {
	hit Hit
	n   int
}

func (x *index) add(m map[string][]Hit, key string, h Hit) {
	if len(m[key]) < MaxHits {
		m[key] = append(m[key], h)
	}
}

// find is where the needles are named: each the lines that quote or assign it, and a quoted
// one's constants' uses after them, in the tree's order. A constant that more than one
// line defines holds the last string asked for.
func (x *index) find(needles map[Needle]bool) map[Needle][]Hit {
	out := map[Needle][]Hit{}
	for n := range needles {
		src := x.strs
		if n.Field {
			src = x.fields
		}
		if hs := src[n.Text]; len(hs) > 0 {
			out[n] = slices.Clone(hs)
		}
	}
	aliases := map[string]string{}
	for _, c := range x.consts {
		if needles[Needle{c.lit, false}] {
			aliases[c.name] = c.lit
		}
	}
	byLit := map[string][]use{}
	for _, name := range slices.Sorted(maps.Keys(aliases)) {
		byLit[aliases[name]] = append(byLit[aliases[name]], x.uses[name]...)
	}
	for lit, us := range byLit {
		slices.SortFunc(us, func(a, b use) int { return a.n - b.n })
		n := Needle{lit, false}
		for _, u := range us[:min(len(us), MaxHits)] {
			out[n] = append(out[n], u.hit)
		}
	}
	return out
}

// tarFiles calls each for every file of a tarball of s's tree, as GitHub serves one (every
// path under one folder), that is in the part of the tree that ships and that want takes,
// with its path from the top of the tree.
func tarFiles(r io.Reader, s Source, want func(file string) bool, each func(file string, r io.Reader) error) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		_, file, _ := strings.Cut(h.Name, "/")
		if h.Typeflag != tar.TypeReg || h.Size > maxFile || !s.holds(file) || !want(file) {
			continue
		}
		if err := each(file, tr); err != nil {
			return err
		}
	}
}

// lines calls each for every line of r, numbered from 1.
func lines(file string, r io.Reader, each func(n int, line string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxFile)
	for n := 1; sc.Scan(); n++ {
		each(n, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// indexNames reads every name a tree of s quotes or assigns, outside tests and comments.
func indexNames(r io.Reader, s Source, lang *language) (*index, error) {
	x := &index{strs: map[string][]Hit{}, fields: map[string][]Hit{}, uses: map[string][]use{}}
	code := func(file string) bool { return lang.code(file) && !lang.testFile(file) }
	err := tarFiles(r, s, code, func(file string, r io.Reader) error {
		var (
			tests     testModule
			lex       = newLexer(s.Lang, false)
			pending   string // a constant whose string is on this line
			fn        int
			importing bool
			enum      string // the enum this line is in
			depth     int    // its braces open
		)
		return lines(file, r, func(n int, raw string) {
			line, _ := lex.line(raw)
			t := strings.TrimSpace(line)
			if (lang.testModules && tests.skip(line)) || t == "" {
				return
			}
			if lang.fnStart(line) {
				fn = n
			}
			h := Hit{file, n, fn}
			// An import names the constant, not the surface's use.
			imported := lang.imported(t, &importing)
			parts := strings.Split(line, `"`)
			for i := 1; i < len(parts)-1; i += 2 {
				x.add(x.strs, parts[i], h)
			}
			if lang.quoted != nil {
				for _, name := range lang.quoted(line) {
					x.add(x.strs, name, h)
				}
			}
			defines := false
			if m := lang.constName.FindStringSubmatch(line); m != nil {
				x.consts = append(x.consts, constant{m[1], m[2]})
				defines = true
			}
			if pending != "" && len(parts) > 2 && strings.HasPrefix(t, `"`) {
				x.consts = append(x.consts, constant{pending, parts[1]})
			}
			pending = ""
			if lang.constHead != nil {
				if m := lang.constHead.FindStringSubmatch(line); m != nil {
					pending = m[1]
				}
			}
			if lang.enums {
				if enum == "" {
					if m := tsEnum.FindStringSubmatch(line); m != nil {
						enum, depth = m[1], 0
					}
				}
				if enum != "" {
					for _, m := range tsEnumVal.FindAllStringSubmatch(line, -1) {
						x.consts = append(x.consts, constant{enum + "." + m[1], m[2]})
						defines = true
					}
					if depth += braces(line); depth <= 0 && strings.Contains(line, "}") {
						enum = ""
					}
				}
			}
			for _, name := range lang.fields(line) {
				x.add(x.fields, name, h)
			}
			if imported || defines {
				return
			}
			type token struct {
				at   int
				name string
			}
			var tokens []token
			for _, re := range lang.constant {
				for _, m := range re.FindAllStringIndex(line, -1) {
					tokens = append(tokens, token{m[0], line[m[0]:m[1]]})
				}
			}
			slices.SortStableFunc(tokens, func(a, b token) int { return a.at - b.at })
			for _, tok := range tokens {
				if len(x.uses[tok.name]) < MaxHits {
					x.uses[tok.name] = append(x.uses[tok.name], use{h, x.n})
				}
				x.n++
			}
		})
	})
	if err != nil {
		return nil, err
	}
	// Only a constant's name is ever looked up: the rest were kept only until the tree said
	// which names are constants.
	defined := map[string]bool{}
	for _, c := range x.consts {
		defined[c.name] = true
	}
	maps.DeleteFunc(x.uses, func(name string, _ []use) bool { return !defined[name] })
	return x, nil
}
