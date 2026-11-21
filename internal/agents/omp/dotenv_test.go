package omp

import (
	"maps"
	"strings"
	"testing"
)

// The .env reads as omp reads it: each want is node:util parseEnv's output for its input, in
// Node 26 and in the Bun runtime omp 19.0.1 ships, which loads the file with it.
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
