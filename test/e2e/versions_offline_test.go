package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// "checked" tests the newest checked build and every release since, the newest few of a
// burst, and nothing older: a quiet night is the one build, a busy one each new release.
func TestSinceChecked(t *testing.T) {
	releases := []string{"0.159.3", "0.160.0", "0.160.1", "0.161.0", "0.162.0", "0.162.1"}
	for _, c := range []struct {
		name    string
		checked []string
		max     int
		want    []string
	}{
		{"a release since", []string{"0.160.0", "0.162.0"}, 5, []string{"0.162.0", "0.162.1"}},
		{"none since", []string{"0.162.1"}, 5, []string{"0.162.1"}},
		{"a burst beyond the cap", []string{"0.159.3"}, 3, []string{"0.159.3", "0.161.0", "0.162.0", "0.162.1"}},
		{"the checked build withdrawn", []string{"0.160.2"}, 5, []string{"0.161.0", "0.162.0", "0.162.1"}},
		{"nothing checked", nil, 5, nil},
		{"only a prerelease checked", []string{"0.163.0-alpha.1"}, 5, nil},
	} {
		if got := sinceChecked(releases, c.checked, c.max); !slices.Equal(got, c.want) {
			t.Errorf("%s: sinceChecked = %v, want %v", c.name, got, c.want)
		}
	}
}

// The history is the published compat history: a build counts as checked by a pass or a
// failure, never by checks that were all skipped; without a history "checked" says why.
func TestCheckedBuildsReadsTheHistory(t *testing.T) {
	releases := []string{"0.161.0", "0.162.0", "0.162.1"}
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, []byte(`[
		{"harness": "codex", "version": "0.161.0", "platform": "darwin/arm64", "capability": "relay.telemetry", "result": "pass"},
		{"harness": "codex", "version": "0.162.0", "platform": "darwin/arm64", "capability": "relay.telemetry", "result": "not run"},
		{"harness": "claude", "version": "2.1.296", "platform": "darwin/arm64", "capability": "relay.telemetry", "result": "fail"}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_E2E_CHECKED", path)
	if got, why := checkedBuildsOn("darwin/arm64", "codex", releases); why != "" || !slices.Equal(got, releases) {
		t.Errorf("codex: %v (%q), want 0.161.0 and all since: 0.162.0 was only skipped", got, why)
	}
	if got, why := checkedBuildsOn("darwin/arm64", "gemini", releases); got != nil || !strings.Contains(why, "checked no gemini release on darwin/arm64") {
		t.Errorf("gemini: %v (%q), want none, and why", got, why)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, why := checkedBuildsOn("darwin/arm64", "codex", releases); got != nil || !strings.Contains(why, "unreadable") {
		t.Errorf("an unreadable history: %v (%q)", got, why)
	}
	t.Setenv("TERMA_E2E_CHECKED", "")
	if got, why := checkedBuildsOn("darwin/arm64", "codex", releases); got != nil || !strings.Contains(why, "TERMA_E2E_CHECKED") {
		t.Errorf("no history: %v (%q)", got, why)
	}
}

// What another platform checked says nothing of this one: the Linux machine job tests the
// newest release every night, and starting the macOS job from it would skip the releases
// between.
func TestCheckedBuildsAreThisPlatforms(t *testing.T) {
	releases := []string{"0.161.0", "0.162.0", "0.162.1", "0.163.0"}
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, []byte(`[
		{"harness": "codex", "version": "0.161.0", "platform": "darwin/arm64", "capability": "relay.telemetry", "result": "pass"},
		{"harness": "codex", "version": "0.163.0", "platform": "linux/amd64", "capability": "relay.telemetry", "result": "pass"}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_E2E_CHECKED", path)
	if got, _ := checkedBuildsOn("darwin/arm64", "codex", releases); !slices.Equal(got, releases) {
		t.Errorf("macOS: %v, want from its own 0.161.0, not Linux's 0.163.0", got)
	}
	if got, _ := checkedBuildsOn("linux/amd64", "codex", releases); !slices.Equal(got, []string{"0.163.0"}) {
		t.Errorf("Linux: %v, want its own 0.163.0", got)
	}
}

// resolve with "checked": the builds it picks, each once beside the installed one, and the
// last three, with a note saying why, where the history says nothing.
func TestResolveChecked(t *testing.T) {
	releases := []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0"}
	list := func() ([]string, error) { return releases, nil }
	var fetched []string
	ensure := func(v string) (string, error) { fetched = append(fetched, v); return "/bin/" + v, nil }
	versions := func(bs []Binary) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Version)
		}
		return out
	}
	path := filepath.Join(t.TempDir(), "history.json")
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if err := os.WriteFile(path, []byte(`[{"harness": "zz", "version": "1.1.0", "platform": "`+platform+`", "capability": "relay.telemetry", "result": "pass"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_E2E_CHECKED", path)
	t.Cleanup(func() { reportMu.Lock(); delete(reportNotes, "yy/versions"); reportMu.Unlock() })
	if got := versions(resolve(t, "zz", "checked", list, ensure)); !slices.Equal(got, []string{"1.1.0", "1.2.0", "1.3.0", "1.4.0"}) {
		t.Errorf("checked: %v", got)
	}
	fetched = nil
	if got := versions(resolve(t, "zz", "1.4.0,checked", list, ensure)); !slices.Equal(got, []string{"1.1.0", "1.2.0", "1.3.0", "1.4.0"}) || countOf(fetched, "1.4.0") != 1 {
		t.Errorf("a build named and checked: %v, fetched %v", got, fetched)
	}
	if got := versions(resolve(t, "yy", "checked", list, ensure)); !slices.Equal(got, []string{"1.2.0", "1.3.0", "1.4.0"}) {
		t.Errorf("no history of yy: %v, want the last three, not zz's 1.1.0 on", got)
	}
	reportMu.Lock()
	notes := slices.Clone(reportNotes["yy/versions"])
	reportMu.Unlock()
	if !slices.ContainsFunc(notes, func(n string) bool {
		return strings.Contains(n, "checked no yy release") && strings.Contains(n, "last 3")
	}) {
		t.Errorf("no note says why yy fell back: %q", notes)
	}
}

func countOf(vs []string, v string) int {
	n := 0
	for _, x := range vs {
		if x == v {
			n++
		}
	}
	return n
}
