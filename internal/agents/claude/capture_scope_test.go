package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

func captureExporter() harness.Exporter {
	return harness.Exporter{
		Endpoint: "https://otel.terma.ai",
		APIKey:   "ter_srv_test",
		Signals:  harness.AllSignals,
	}
}

func findConflict(conflicts []harness.Conflict, key string) *harness.Conflict {
	for i := range conflicts {
		if conflicts[i].Key == key {
			return &conflicts[i]
		}
	}
	return nil
}

// A shell export of a capture key as off is reported: terma always means to capture.
func TestCaptureConflictsReportsShellOverride(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv(otelLogUserPrompts, "false")
	got := captureConflictsIn(configDir, exporter{dir: configDir}.layer())
	c := findConflict(got, otelLogUserPrompts)
	if c == nil {
		t.Fatalf("expected %s to be reported, got %+v", otelLogUserPrompts, got)
	}
	if !c.Advisory {
		t.Error("a capture override must be advisory: it discloses nothing and breaks no export")
	}
	if c.Credential {
		t.Error("a capture override carries no credential")
	}
	if c.Scope != harness.ScopeEnvironment {
		t.Errorf("scope = %q, want %q", c.Scope, harness.ScopeEnvironment)
	}
}

func TestCaptureConflictsIgnoresAgreeingValue(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv(otelLogUserPrompts, "1")
	if got := captureConflictsIn(configDir, exporter{dir: configDir}.layer()); len(got) != 0 {
		t.Fatalf("expected no conflict when the export agrees, got %+v", got)
	}
}

// A project settings file that turns capture off outranks the user file terma writes.
func TestCaptureConflictsReportsProjectOverride(t *testing.T) {
	configDir := t.TempDir()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"),
		[]byte(`{"env":{"OTEL_LOG_TOOL_CONTENT":"0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	got := captureConflictsIn(configDir, exporter{dir: configDir}.layer())
	c := findConflict(got, otelLogToolContent)
	if c == nil {
		t.Fatalf("expected %s to be reported, got %+v", otelLogToolContent, got)
	}
	if c.Scope != harness.ScopeProject || !c.Advisory {
		t.Errorf("got scope=%q advisory=%v, want project/advisory", c.Scope, c.Advisory)
	}
}

// A capture override is reported, never blocking.
func TestCaptureConflictsNeverBlock(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv(otelLogUserPrompts, "0")
	t.Setenv(otelLogToolContent, "0")
	for _, c := range captureConflictsIn(configDir, exporter{dir: configDir}.layer()) {
		if !c.Advisory {
			t.Fatalf("%s would block a connect", c.Key)
		}
	}
}

// render writes every capture key on: content always goes to the relay, which withholds
// it per the team's policy, so an earlier exclusion never survives the next connect.
func TestRenderAlwaysCapturesContent(t *testing.T) {
	t.Parallel()
	on := exporter{}.render(captureExporter())
	for _, key := range captureKeys {
		if on[key] != "1" {
			t.Errorf("%s = %q, want \"1\"", key, on[key])
		}
	}
}
