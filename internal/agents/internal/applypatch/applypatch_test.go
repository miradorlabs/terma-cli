package applypatch

import (
	"slices"
	"testing"
)

func TestPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"none", "go build ./...", nil},
		{"add", "*** Begin Patch\n*** Add File: a/b.go\n*** End Patch", []string{"a/b.go"}},
		{"update and delete", "*** Update File: x.go\n*** Delete File: y.go", []string{"x.go", "y.go"}},
		// A rename touches both paths, and the commit will carry both.
		{"rename", "*** Update File: old.go\n*** Move to: new.go", []string{"old.go", "new.go"}},
		{"indented heredoc", "  *** Add File: spaced.go  ", []string{"spaced.go"}},
		{"header with no path", "*** Add File:", nil},
		{"paths with spaces", "*** Add File: dir with space/f.go", []string{"dir with space/f.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Paths(tc.text); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
