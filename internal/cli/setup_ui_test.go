package cli

import (
	"slices"
	"strings"
	"testing"
)

// flat is s with every run of whitespace one space, for matching wrapped output.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestWrapStep(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		width      int
		want       []string
	}{
		{"wraps at width", "one two three four", 9, []string{"one two", "three", "four"}},
		{"keeps a command whole", "then run `terma setup --org acme` now", 12, []string{"then run", "`terma setup --org acme`", "now"}},
		{"keeps line breaks", "first line\nsecond", 80, []string{"first line", "second"}},
	} {
		if got := wrapStep(tc.step, tc.width); !slices.Equal(got, tc.want) {
			t.Errorf("%s: wrapStep(%q, %d) = %q, want %q", tc.name, tc.step, tc.width, got, tc.want)
		}
	}
}
