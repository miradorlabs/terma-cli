package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

func TestDoctorChecksClaudeEmissionSettings(t *testing.T) {
	const endpoint = "https://otel.example.test"
	for _, tc := range []struct {
		name          string
		globalSignals []harness.Signal
		shared, local map[string]string
		brokenLocal   bool
		wantFailure   bool
		wantDetail    string
	}{
		{name: "legacy repos-only connect without policy", wantFailure: true, wantDetail: "send nothing"},
		{name: "repository enables globally disabled logs", shared: map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}},
		{name: "partial policy inherits the traces beta flag", globalSignals: []harness.Signal{harness.SignalTraces}, shared: map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}},
		{name: "all exporters explicitly disabled", globalSignals: harness.AllSignals,
			shared: map[string]string{"OTEL_TRACES_EXPORTER": "none", "OTEL_LOGS_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none"}, wantFailure: true, wantDetail: ".claude/settings.json"},
		{name: "private settings override working policy", shared: map[string]string{"OTEL_LOGS_EXPORTER": "otlp"},
			local: map[string]string{"OTEL_LOGS_EXPORTER": "none"}, wantFailure: true, wantDetail: "settings.local.json"},
		{name: "private settings enable telemetry", local: map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}},
		{name: "repository master switch disables everything", globalSignals: harness.AllSignals,
			local: map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "0"}, wantFailure: true, wantDetail: "CLAUDE_CODE_ENABLE_TELEMETRY"},
		{name: "traces exporter without beta switch emits nothing", shared: map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, wantFailure: true, wantDetail: "no OTLP telemetry signals"},
		{name: "metrics still emit with traces disabled", globalSignals: harness.AllSignals,
			local: map[string]string{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "0", "OTEL_LOGS_EXPORTER": "none"}},
		{name: "malformed private settings cannot pass", globalSignals: harness.AllSignals, brokenLocal: true, wantFailure: true, wantDetail: "could not read effective telemetry settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := installRepo(t)
			t.Setenv(shim.WrapperEnv, "")
			h := harness.Claude{}
			if err := h.Connect(harness.Exporter{Endpoint: endpoint, APIKey: testServerKey, Signals: tc.globalSignals}, false); err != nil {
				t.Fatal(err)
			}
			writeDoctorEnv(t, filepath.Join(repo, ".claude/settings.json"), tc.shared)
			localPath := filepath.Join(repo, ".claude/settings.local.json")
			writeDoctorEnv(t, localPath, tc.local)
			if tc.brokenLocal {
				if err := os.MkdirAll(filepath.Dir(localPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(localPath, []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			v := judgeHarness(gatherHarness(h, testProjectID, repo), endpoint, testProjectID)
			v.name, v.displayName = h.Name(), h.DisplayName()
			check := doctorHarnessCheck([]harnessVerdict{v}, endpoint, testProjectID, true)
			if (check.Status == doctor.Fail) != tc.wantFailure || !strings.Contains(check.Detail, tc.wantDetail) {
				t.Fatalf("doctor = %+v; want failure %v containing %q", check, tc.wantFailure, tc.wantDetail)
			}
			if !tc.wantFailure && check.Status != doctor.Pass {
				t.Fatalf("working export did not pass: %+v", check)
			}
			status, ok := statusAgent(v, true)
			if ok == tc.wantFailure {
				t.Fatalf("status disagrees with doctor: %s", status)
			}
			if tc.wantFailure {
				// Another installed agent must not conceal a silent Claude setup.
				healthy := harnessVerdict{displayName: "Codex", route: routeGlobal}
				check = doctorHarnessCheck([]harnessVerdict{healthy, v}, endpoint, testProjectID, true)
				check.Key = doctor.KeyHarness
				if !doctor.Build([]doctor.Check{check}).Failed() || check.Fix == "" {
					t.Fatalf("silent Claude should fail with a remedy even alongside Codex: %+v", check)
				}
			}
		})
	}
}

func writeDoctorEnv(t *testing.T, path string, env map[string]string) {
	t.Helper()
	if env == nil {
		return
	}
	data, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorKeepsWorkingExportWhenAnotherAgentNeedsRouting(t *testing.T) {
	check := doctorHarnessCheck([]harnessVerdict{
		{displayName: "Claude Code", route: routePending},
		{displayName: "Codex", route: routeGlobal},
	}, "https://otel.example.test", testProjectID, true)
	if check.Status != doctor.Warn || check.Ready != 1 || check.Of != 2 {
		t.Fatalf("working Codex export should survive Claude's warning: %+v", check)
	}
}

func TestDoctorChecksLiveRouteSignals(t *testing.T) {
	for _, signals := range [][]string{nil, {"logs"}} {
		t.Run(strings.Join(signals, ","), func(t *testing.T) {
			repo := installRepo(t)
			t.Setenv(shim.WrapperEnv, "wrapper")
			// The routed --settings document outranks even a disabled local policy.
			writeDoctorEnv(t, filepath.Join(repo, ".claude/settings.local.json"), map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "0"})
			if err := shim.SaveRecord(shim.Record{ProjectID: testProjectID, Endpoint: "https://otel.example.test", Signals: signals, Harnesses: []string{"claude"}}); err != nil {
				t.Fatal(err)
			}
			if err := keystore.SetFor("claude", testProjectID, testServerKey); err != nil {
				t.Fatal(err)
			}
			v := judgeHarness(gatherHarness(harness.Claude{}, testProjectID, repo), "https://otel.example.test", testProjectID)
			v.name, v.displayName = "claude", "Claude Code"
			check := doctorHarnessCheck([]harnessVerdict{v}, "https://otel.example.test", testProjectID, true)
			if (check.Status == doctor.Pass) != (len(signals) > 0) {
				t.Fatalf("route signals %v: %+v", signals, check)
			}
		})
	}
}

func TestDoctorChecksOpenCodeRepositoryPolicy(t *testing.T) {
	const endpoint = "https://otel.example.test"
	for _, signals := range [][]harness.Signal{nil, {harness.SignalLogs}} {
		t.Run(joinSignals(signals), func(t *testing.T) {
			repo := installRepo(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			h := harness.OpenCode{}
			if err := h.Connect(harness.Exporter{Endpoint: endpoint, APIKey: testServerKey, Signals: harness.AllSignals}, false); err != nil {
				t.Fatal(err)
			}
			if err := h.Local(repo).Connect(harness.Exporter{Signals: signals}, false); err != nil {
				t.Fatal(err)
			}
			if err := keystore.SetFor("opencode", testProjectID, testServerKey); err != nil {
				t.Fatal(err)
			}
			v := judgeHarness(gatherHarness(h, testProjectID, repo), endpoint, testProjectID)
			v.name, v.displayName = h.Name(), h.DisplayName()
			check := doctorHarnessCheck([]harnessVerdict{v}, endpoint, testProjectID, true)
			if (check.Status == doctor.Pass) != (len(signals) > 0) {
				t.Fatalf("repository signals %v: %+v", signals, check)
			}
		})
	}
}
