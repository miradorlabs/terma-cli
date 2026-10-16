package opencode

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// withMajor makes the installed OpenCode report major, for the length of the test.
func withMajor(t *testing.T, major int) {
	t.Helper()
	saved := installedMajor
	installedMajor = func() int { return major }
	t.Cleanup(func() { installedMajor = saved })
}

// Setup names terma by the path it was given: OpenCode 2's background server runs the
// hook with whatever PATH its first client had.
func TestOpenCodeRelayPluginRunsTermaByPath(t *testing.T) {
	_, path := opencodeIn(t)
	a := Agent{ConfigDir: t.TempDir()}
	hook := []string{"/usr/local/bin/terma", "hook"}
	if _, err := a.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: hook}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := readPluginConfig(data)
	if !ok || !reflect.DeepEqual(cfg.HookCommand, hook) {
		t.Fatalf("hook command = %+v", cfg)
	}
}

func TestOpenCodeRelayProblemNamesAPluginOpenCode2CannotUse(t *testing.T) {
	_, path := opencodeIn(t)
	a := Agent{ConfigDir: t.TempDir()}
	relay := agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: []string{"/usr/local/bin/terma", "hook"}}
	if _, err := a.ConfigureRelay(t.Context(), relay); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// An earlier terma's plugin: OpenCode 1's function export, run by a bare name.
	cfg, _ := readPluginConfig(current)
	line := opencodeConfigLine.Find(current)
	old := string(line) + "\nexport const TermaPlugin = async ({ directory, worktree }) => ({})\n"
	bare := strings.Replace(string(current), `"hookCommand":["/usr/local/bin/terma","hook"]`, `"hookCommand":["terma","hook"]`, 1)
	if cfg == nil || bare == string(current) {
		t.Fatal("the written plugin is not what the test expects")
	}

	for _, tc := range []struct {
		name    string
		major   int
		plugin  string
		problem string
	}{
		{"OpenCode 1 runs either", 1, old, ""},
		{"OpenCode unknown", 0, old, ""},
		{"OpenCode 2 with the current plugin", 2, string(current), ""},
		{"OpenCode 2 with OpenCode 1's plugin", 2, old, "written for OpenCode 1"},
		{"OpenCode 2 with terma named bare", 2, bare, `runs "terma" by name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withMajor(t, tc.major)
			if err := os.WriteFile(path, []byte(tc.plugin), 0o644); err != nil {
				t.Fatal(err)
			}
			detail, fix, problem := a.RelayProblem()
			if problem != (tc.problem != "") || !strings.Contains(detail, tc.problem) {
				t.Fatalf("RelayProblem = %q, %v; want one saying %q", detail, problem, tc.problem)
			}
			if problem && (fix != "terma setup" || !strings.Contains(detail, "terma setup")) {
				t.Errorf("fix = %q; detail %q", fix, detail)
			}
		})
	}

	// No plugin, or an inert one, is not this check's to report.
	withMajor(t, 2)
	if err := os.WriteFile(path, []byte(opencodePluginSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, problem := a.RelayProblem(); problem {
		t.Error("an inert plugin was reported")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, problem := a.RelayProblem(); problem {
		t.Error("a missing plugin was reported")
	}
}
