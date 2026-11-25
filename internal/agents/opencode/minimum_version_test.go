package opencode

import (
	"strings"
	"testing"
)

// Each major has its own minimum; a version terma cannot read, or a major it lists no
// minimum for, is not too old.
func TestTooOldVersionByMajor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		version string
		old     bool
		minimum string
	}{
		{"1.15.9", true, "1.16.0"},
		{"1.15.0", true, "1.16.0"},
		{"1.2.11", true, "1.16.0"},
		{"1.16.0", false, ""},
		{"1.19.6", false, ""},
		{"2.0.0", false, ""},
		{"2.4.1", false, ""},
		{"v1.16.0", false, ""},
		{"1.16.0-beta.1", false, ""},
		{"", false, ""},
		{"nightly", false, ""},
		{"0.9.0", false, ""},
		{"3.0.0", false, ""},
	} {
		why, old := tooOldVersion(c.version)
		if old != c.old {
			t.Errorf("%q: too old %v, want %v (%s)", c.version, old, c.old, why)
			continue
		}
		if old && (!strings.Contains(why, "OpenCode "+c.minimum+" or later is required (found "+c.version+")") || !strings.Contains(why, "terma's plugin exports nothing")) {
			t.Errorf("%q: said %q", c.version, why)
		}
	}
}
