package style

import (
	"slices"
	"testing"
)

// Wrap breaks at spaces within the width, never inside a quoted command, and not at all
// without a width.
func TestWrap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		s     string
		width int
		want  []string
	}{
		{"run `terma setup --relay-addr host:port` now", 12, []string{"run", "`terma setup --relay-addr host:port`", "now"}},
		{"one two three", 7, []string{"one two", "three"}},
		{"one two three", 0, []string{"one two three"}},
		{"Mirador › Platform", 18, []string{"Mirador › Platform"}},
	} {
		if got := Wrap(tc.s, tc.width); !slices.Equal(got, tc.want) {
			t.Errorf("Wrap(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.want)
		}
	}
}
