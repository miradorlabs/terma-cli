package e2e

import (
	"os"
	"path/filepath"
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
	if got, why := checkedBuilds("codex", releases); why != "" || !slices.Equal(got, releases) {
		t.Errorf("codex: %v (%q), want 0.161.0 and all since: 0.162.0 was only skipped", got, why)
	}
	if got, why := checkedBuilds("gemini", releases); got != nil || !strings.Contains(why, "checked no gemini build") {
		t.Errorf("gemini: %v (%q), want none, and why", got, why)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, why := checkedBuilds("codex", releases); got != nil || !strings.Contains(why, "unreadable") {
		t.Errorf("an unreadable history: %v (%q)", got, why)
	}
	t.Setenv("TERMA_E2E_CHECKED", "")
	if got, why := checkedBuilds("codex", releases); got != nil || !strings.Contains(why, "TERMA_E2E_CHECKED") {
		t.Errorf("no history: %v (%q)", got, why)
	}
}
