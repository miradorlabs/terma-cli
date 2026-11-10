package opencode

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// withVersion makes the installed OpenCode report version ("" for none), for the length
// of the test.
func withVersion(t *testing.T, version string) {
	t.Helper()
	saved := installedVersion
	installedVersion = func() string { return version }
	t.Cleanup(func() { installedVersion = saved })
}

// fakeTerma is an executable standing in for terma, which a repaired hook command names.
func fakeTerma(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	saved := termaExecutable
	termaExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { termaExecutable = saved })
	return path
}

func relayConfig(hook string) agents.RelayConfig {
	return agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: []string{hook, "hook"}}
}

func readHook(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := readPluginConfig(data)
	if !ok {
		t.Fatal("no configuration")
	}
	return cfg.HookCommand
}

func TestOpenCodeSupportedVersions(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]bool{
		"": true, "1.0.224": false, "1.2.10": false, "1.2.11": true, "1.3.0": true, "1.19.2": true, "2.0.30": true, "1.2": false,
	} {
		if supported(version) != want {
			t.Errorf("supported(%q) = %v", version, !want)
		}
	}
}

// Setup names terma by the path it was given: OpenCode 2's background server runs the
// hook with whatever PATH its first client had.
func TestOpenCodeRelayPluginRunsTermaByPath(t *testing.T) {
	h, path := opencodeIn(t)
	a := Agent{ConfigDir: t.TempDir()}
	if _, err := a.ConfigureRelay(t.Context(), relayConfig(h.hook[0])); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, path); !reflect.DeepEqual(got, h.hook) {
		t.Fatalf("hook command = %v", got)
	}
}

// Under an OpenCode too old for it, setup writes no plugin, which would keep OpenCode from
// starting, and says why.
func TestOpenCodeRelaySkipsAnOpenCodeTooOld(t *testing.T) {
	h, path := opencodeIn(t)
	withVersion(t, "1.2.10")
	res, err := Agent{ConfigDir: t.TempDir()}.ConfigureRelay(t.Context(), relayConfig(h.hook[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pending || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "1.2.11") {
		t.Fatalf("result = %+v", res)
	}
	if exists(path) {
		t.Fatal("a plugin was written for an OpenCode that cannot load it")
	}
}

func TestOpenCodeRelayProblemNamesAPluginThatWillNotReport(t *testing.T) {
	h, path := opencodeIn(t)
	a := Agent{ConfigDir: t.TempDir()}
	if _, err := a.ConfigureRelay(t.Context(), relayConfig(h.hook[0])); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withHook := func(src []byte, hook string) string {
		quoted := strings.ReplaceAll(h.hook[0], `\`, `\\`)
		out := strings.Replace(string(src), `"hookCommand":["`+quoted+`","hook"]`, `"hookCommand":["`+strings.ReplaceAll(hook, `\`, `\\`)+`","hook"]`, 1)
		if out == string(src) {
			t.Fatal("the written plugin is not what the test expects")
		}
		return out
	}
	// An earlier terma's plugin: OpenCode 1's function export, run by a bare name.
	old := string(opencodeConfigLine.Find([]byte(withHook(current, "terma")))) + "\nexport const TermaPlugin = async ({ directory, worktree }) => ({})\n"
	bare := withHook(current, "terma")
	gone := withHook(current, filepath.Join(t.TempDir(), "gone", "terma"))

	for _, tc := range []struct {
		name, version, plugin, problem string
	}{
		{"OpenCode 1 runs either", "1.19.2", old, ""},
		{"OpenCode unknown", "", old, ""},
		{"OpenCode too old", "1.2.10", string(current), "older than 1.2.11"},
		{"OpenCode 2 with the current plugin", "2.0.30", string(current), ""},
		{"OpenCode 2 with OpenCode 1's plugin", "2.0.30", old, "written for OpenCode 1"},
		{"OpenCode 2 with terma named bare", "2.0.30", bare, `runs "terma" by name`},
		{"terma gone, under OpenCode 1", "1.19.2", gone, "not there now"},
		{"terma gone, under OpenCode 2", "2.0.30", gone, "not there now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withVersion(t, tc.version)
			if err := os.WriteFile(path, []byte(tc.plugin), 0o644); err != nil {
				t.Fatal(err)
			}
			detail, fix, problem := a.RelayProblem()
			if problem != (tc.problem != "") || !strings.Contains(detail, tc.problem) {
				t.Fatalf("RelayProblem = %q, %v; want one saying %q", detail, problem, tc.problem)
			}
			if problem && (!strings.Contains(fix, "terma setup") || !strings.Contains(detail, "terma setup")) {
				t.Errorf("fix = %q; detail %q", fix, detail)
			}
		})
	}

	// No plugin, or an inert one, is not this check's to report.
	withVersion(t, "2.0.30")
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

// Refresh points a hook command at this terma when the one it names is bare or gone, and
// leaves the plugin of an OpenCode too old for this build's as it is.
func TestRefreshPluginRepairsTheHookAndSparesAnOldOpenCode(t *testing.T) {
	h, path := opencodeIn(t)
	for _, hook := range []string{"terma", filepath.Join(t.TempDir(), "gone", "terma")} {
		e := exporter{dir: h.dir, hook: []string{hook, "hook"}}
		if err := e.Connect(opencodeExporter(t, e, false), false); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := h.RefreshPlugin(); err != nil || !changed {
			t.Fatalf("%s: changed=%v err=%v", hook, changed, err)
		}
		if got := readHook(t, path); !reflect.DeepEqual(got, h.hook) {
			t.Fatalf("%s: hook command = %v, want %v", hook, got, h.hook)
		}
	}

	withVersion(t, "1.1.65")
	old := []byte(string(opencodeConfigLine.Find(mustRead(t, path))) + "\nexport const TermaPlugin = async () => ({})\n")
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := h.RefreshPlugin(); err != nil || changed {
		t.Fatalf("old OpenCode: changed=%v err=%v", changed, err)
	}
	if got := mustRead(t, path); string(got) != string(old) {
		t.Fatal("the plugin an old OpenCode loads was rewritten")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// One doctor asks each OpenCode build its version once.
func TestOpenCodeVersionIsAskedOncePerBuild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake OpenCode is a shell script")
	}
	bin := t.TempDir()
	count := filepath.Join(bin, "count")
	script := "#!/bin/sh\necho x >> '" + count + "'\necho 'opencode v2.0.30'\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for range 3 {
		if v := (exporter{}).Detect(t.Context()).Version; v != "2.0.30" {
			t.Fatalf("version = %q", v)
		}
	}
	if n := strings.Count(string(mustRead(t, count)), "x"); n != 1 {
		t.Fatalf("opencode --version ran %d times", n)
	}
}
