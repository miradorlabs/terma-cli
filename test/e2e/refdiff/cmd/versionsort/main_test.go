package main

import (
	"strings"
	"testing"
)

// The command sorts as refdiff ranks, the versions GNU sort -V reads otherwise among them: a
// prerelease with a dash in it (rc-1), and alphanumeric identifiers after numeric ones.
func TestSortVersions(t *testing.T) {
	in := "1.0.0\n1.0.0-rc-1\n\n1.0.0-alpha.beta\n0.2.1\n0.2.1-rc.10\n0.2.1-rc.9\n1.0.0-alpha.1\n1.0.0-alpha\n1.0.0-1\n0.2.1\n"
	for unique, want := range map[bool]string{
		false: "0.2.1-rc.9\n0.2.1-rc.10\n0.2.1\n0.2.1\n1.0.0-1\n1.0.0-alpha\n1.0.0-alpha.1\n1.0.0-alpha.beta\n1.0.0-rc-1\n1.0.0\n",
		true:  "0.2.1-rc.9\n0.2.1-rc.10\n0.2.1\n1.0.0-1\n1.0.0-alpha\n1.0.0-alpha.1\n1.0.0-alpha.beta\n1.0.0-rc-1\n1.0.0\n",
	} {
		var out strings.Builder
		if err := sortVersions(strings.NewReader(in), &out, unique); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Errorf("unique %v:\n got %q\nwant %q", unique, out.String(), want)
		}
	}
}
