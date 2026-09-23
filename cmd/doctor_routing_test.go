package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

func TestShellRoutingCheckExplainsActivation(t *testing.T) {
	const endpoint = "https://otel.example.test"
	for _, tc := range []struct {
		name, activation, rc, detail, fix string
		routed, keyless, noExport         bool
		want                              doctor.Status
	}{
		{name: "never opted into shell integration", detail: "per-repo routing is not configured", fix: "terma install", want: doctor.Warn},
		{name: "declined PATH setup", routed: true, detail: "PATH setup is missing", fix: "terma install", want: doctor.Warn},
		{name: "shell has not been restarted", routed: true, rc: "last", detail: "this shell has not activated it", fix: "open a new terminal", want: doctor.Warn},
		{name: "later PATH entry bypasses shim", routed: true, rc: "overtaken", detail: "a later PATH setting", fix: "moves terma's line to the end", want: doctor.Warn},
		{name: "wrapper loaded", routed: true, activation: "wrapper", detail: "shell wrapper active", want: doctor.Pass},
		{name: "shim on PATH", routed: true, activation: "shim", detail: "PATH shim active", want: doctor.Pass},
		{name: "active wrapper without project routing", activation: "wrapper", detail: "per-repo routing is not configured", fix: "terma install", want: doctor.Warn},
		{name: "route lacks project key", routed: true, keyless: true, activation: "wrapper", detail: "per-repo routing is not configured", fix: "terma install", want: doctor.Warn},
		{name: "inactive routing has no global fallback", routed: true, noExport: true, detail: "telemetry is not configured to reach this project", fix: "terma install", want: doctor.Warn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := installRepo(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SHELL", "/bin/zsh")
			t.Setenv("ZDOTDIR", "")
			t.Setenv(shim.WrapperEnv, "")
			fakeClaudeOnPath(t)
			h := harness.Claude{}
			signals := harness.AllSignals
			if tc.noExport {
				signals = nil
			}
			if err := h.Connect(harness.Exporter{Endpoint: endpoint, APIKey: testServerKey, Signals: signals}, false); err != nil {
				t.Fatal(err)
			}
			if tc.routed {
				if err := shim.SaveRecord(shim.Record{ProjectID: testProjectID, Endpoint: endpoint, Signals: []string{"logs"}, Harnesses: []string{"claude"}}); err != nil {
					t.Fatal(err)
				}
				if !tc.keyless {
					if err := keystore.SetFor("claude", testProjectID, testServerKey); err != nil {
						t.Fatal(err)
					}
				}
			}
			bin, err := shim.ShimBinDir()
			if err != nil {
				t.Fatal(err)
			}
			rc, _ := shim.ShellRC()
			if tc.rc != "" {
				if _, err := rc.Ensure(bin); err != nil {
					t.Fatal(err)
				}
				if tc.rc == "overtaken" {
					data, err := os.ReadFile(rc.Path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(rc.Path, append(data, []byte("export PATH=/usr/local/bin:$PATH\n")...), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch tc.activation {
			case "wrapper":
				t.Setenv(shim.WrapperEnv, "wrapper")
			case "shim":
				if _, err := shim.InstallShims([]string{"claude"}); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			before, _ := os.ReadFile(rc.Path)
			v := judgeHarness(gatherHarness(h, testProjectID, repo), endpoint, testProjectID)
			v.name, v.displayName = h.Name(), h.DisplayName()
			check := shellRoutingCheck([]harnessVerdict{v}, true, nil)
			if check.Status != tc.want || !strings.Contains(check.Detail, tc.detail) || !strings.Contains(check.Fix, tc.fix) {
				t.Fatalf("check = %+v; want %s containing %q, fix %q", check, tc.want, tc.detail, tc.fix)
			}
			if tc.want == doctor.Warn && !tc.noExport && !strings.Contains(check.Detail, "telemetry uses global settings") {
				t.Fatalf("inactive routing should explain that global export still works: %+v", check)
			}
			after, err := os.ReadFile(rc.Path)
			if string(after) != string(before) || (tc.rc == "" && !os.IsNotExist(err)) {
				t.Fatal("diagnostic changed the shell startup file")
			}
		})
	}
}

func TestDoctorAndStatusShowMissingShellOptIn(t *testing.T) {
	installRepo(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	t.Setenv(shim.WrapperEnv, "")
	fakeClaudeOnPath(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor"); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "connect", "claude", "--api-key", testServerKey, "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"doctor", "--skip-commit"}, {"status"}} {
		out, _ := runTerma(t, args...)
		for _, want := range []string{"per-repo routing is not configured", "shell integration inactive", "telemetry uses global settings", "PATH setup is missing"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s omitted %q:\n%s", args[0], want, out)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".zshrc")); !os.IsNotExist(err) {
		t.Fatal("doctor/status should not opt into shell integration")
	}
}

func TestShellRoutingCheckSkipsAgentsThatDoNotNeedIt(t *testing.T) {
	for _, tc := range []struct {
		bound bool
		name  string
		mine  []string
	}{
		{false, "claude", nil},
		{true, "opencode", nil},
		{true, "codex", []string{"claude"}},
	} {
		check := shellRoutingCheck([]harnessVerdict{{name: tc.name}}, tc.bound, tc.mine)
		if check.Status != doctor.Skip {
			t.Fatalf("unexpected routing requirement for %+v: %+v", tc, check)
		}
	}
}
