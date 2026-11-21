package omp

import (
	"maps"
	"strings"
	"testing"
)

// The .env reads as omp reads it: each want is node:util parseEnv's output for its input in
// the Bun 1.4.4 runtime OMP 19.1.0 ships (`BUN_BE_BUN=1 omp`), less the names OMP drops.
func TestEnvFileReadsAsParseEnv(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   string
		want map[string]string
	}{
		"plain":            {"A=1", map[string]string{"A": "1"}},
		"hash, no space":   {"A=a#b", map[string]string{"A": "a"}},
		"hash after space": {"A=a #b", map[string]string{"A": "a"}},
		"export prefix":    {"export A=x", map[string]string{"A": "x"}},
		"single quotes":    {"A='a b # c'", map[string]string{"A": "a b # c"}},
		"double quotes":    {`A="a\nb"`, map[string]string{"A": "a\nb"}},
		"backticks":        {"A=`x'y`", map[string]string{"A": "x'y"}},
		"multiline":        {"A=\"line1\nline2\"\nB=2", map[string]string{"A": "line1\nline2", "B": "2"}},
		"crlf":             {"A=1\r\nB=2\r\n", map[string]string{"A": "1", "B": "2"}},
		"unclosed quote":   {"A=\"abc\nB=2", map[string]string{"A": `"abc`, "B": "2"}},
		"last one wins":    {"A=1\nA=2", map[string]string{"A": "2"}},
		"spaces":           {"  A = v  ", map[string]string{"A": "v"}},
		"comment line":     {"# A=1\nB=2", map[string]string{"B": "2"}},
		"empty value":      {"A=", map[string]string{"A": ""}},
		"no expansion":     {"A=$HOME", map[string]string{"A": "$HOME"}},
		"escaped double quote spans lines": {"A=\"one\\\"\nOTEL_EXPORTER_OTLP_ENDPOINT=x\ntwo\"\nB=c",
			map[string]string{"A": "one\\\"\nOTEL_EXPORTER_OTLP_ENDPOINT=x\ntwo", "B": "c"}},
		"escaped single quote spans lines": {"A='one\\'\nB=x\ntwo'", map[string]string{"A": "one\\'\nB=x\ntwo"}},
		"escaped backtick spans lines":     {"A=`one\\`\nB=x\ntwo`", map[string]string{"A": "one\\`\nB=x\ntwo"}},
		"escaped backslash closes":         {"A=\"a\\\\\"\nB=x\"", map[string]string{"A": `a\\`, "B": `x"`}},
		"escapes decode in pairs":          {`A="a\\nb\rc\td"`, map[string]string{"A": "a\\\\nb\rc\\td"}},
		"text after the closing quote":     {"A=\"x\"y B=1\nC=z", map[string]string{"A": "x", "C": "z"}},
		"colon separator":                  {"A: \"x\nC=1\n\"", map[string]string{"A": "x\nC=1\n"}},
		"colon needs a space":              {"A:\"x\nC=1\n\"", map[string]string{"C": "1"}},
		"value on the next line":           {"A=\n\"x\nC=1\n\"", map[string]string{"A": "x\nC=1\n"}},
		"separator on the next line":       {"A\n=1", map[string]string{"A": "1"}},
		"a name omp drops keeps its value": {"A-B=\"x\nC=1\n\"\n1A=2", map[string]string{}},
		"crlf in quotes":                   {"A=\"x\r\nB=1\"\r\nC=2", map[string]string{"A": "x\nB=1", "C": "2"}},
		"ascii trim only":                  {"A=\u00a0x\u00a0", map[string]string{"A": "\u00a0x\u00a0"}},
		"byte order mark":                  {"\ufeffA=1", map[string]string{"A": "1"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &envFile{lines: strings.Split(strings.TrimSuffix(c.in, "\n"), "\n")}
			if got := f.values(); !maps.Equal(got, c.want) {
				t.Fatalf("values(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
