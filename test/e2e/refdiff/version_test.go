package refdiff

import (
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Versions rank as semantic versioning ranks them, and no two different versions are equal.
func TestVersionOrder(t *testing.T) {
	// Each before the next.
	ordered := []string{
		"0.2.1-rc.1", "0.2.1-rc.9", "0.2.1-rc.10", "0.2.1", "0.2.2-rc.1",
		"1.0.0-1", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
		"1.0.1", "1.0.5", "1.0.5+a", "1.0.5+b", "1.0.5+b.2", "1.0.10",
		"1.2", "1.2.0", "1.2.1",
		"2.1.9", "2.1.10", "2.1.300",
		"26.1029.70936", "26.1030.1",
	}
	for i := range ordered {
		for j := range ordered {
			if got, want := VersionLess(ordered[i], ordered[j]), i < j; got != want {
				t.Errorf("VersionLess(%q, %q) = %v, want %v", ordered[i], ordered[j], got, want)
			}
		}
		if VersionCompare(ordered[i], ordered[i]) != 0 {
			t.Errorf("%s is not itself", ordered[i])
		}
	}
	// Whatever order they come in, a sort makes the one order.
	for range 20 {
		vs := slices.Clone(ordered)
		rand.Shuffle(len(vs), func(i, j int) { vs[i], vs[j] = vs[j], vs[i] })
		slices.SortFunc(vs, VersionCompare)
		if !slices.Equal(vs, ordered) {
			t.Fatalf("sorted = %q", vs)
		}
	}
	// Build metadata takes no part in precedence: 1.0.5+b is 1.0.5, after 1.0.1.
	if !VersionLess("1.0.1", "1.0.5+b") || VersionLess("1.0.5+b", "1.0.1") {
		t.Error("1.0.5+b ranks below 1.0.1")
	}
}

// The frequent check's shell sorts versions as VersionCompare does, for the versions the
// registries and update feeds publish: dotted releases, prereleases (DeepSeek Harness's
// 0.2.1-rc.N, which sort -V alone puts after 0.2.1), and the desktop apps' releases.
func TestShellVersionSortAgrees(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for _, script := range []string{"../reference/new-builds.sh", "../reference/backfill.sh"} {
		data, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		var def string
		for line := range strings.Lines(string(data)) {
			if strings.HasPrefix(line, "vsort()") {
				def = line
			}
		}
		if def == "" {
			t.Fatalf("%s defines no vsort", script)
		}
		want := []string{"0.2.1-rc.1", "0.2.1-rc.5", "0.2.1-rc.9", "0.2.1-rc.10", "0.2.1", "0.2.2-rc.1",
			"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-beta", "1.0.0", "1.2", "1.2.0", "2.1.9", "2.1.10", "2.1.300", "26.1029.70936", "26.1030.1"}
		in := slices.Clone(want)
		rand.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
		cmd := exec.Command("bash", "-c", def+"\nvsort")
		cmd.Stdin = strings.NewReader(strings.Join(in, "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("sort -V: %v", err)
		}
		got := strings.Fields(string(out))
		if !slices.Equal(got, want) {
			t.Errorf("%s's vsort = %q, want %q", script, got, want)
		}
		if !slices.IsSortedFunc(want, VersionCompare) {
			t.Errorf("VersionCompare disagrees with %q", want)
		}
	}
}
