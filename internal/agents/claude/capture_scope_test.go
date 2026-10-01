package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

func captureExporter() harness.Exporter {
	return harness.Exporter{
		Endpoint:           "https://otel.terma.ai",
		APIKey:             "ter_srv_test",
		Signals:            harness.AllSignals,
		IncludePrompts:     true,
		IncludeToolContent: true,
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

// A shell export of a capture key as off is reported when the connect means to capture.
func TestCaptureConflictsReportsShellOverride(t *testing.T) {
	t.Setenv(otelLogUserPrompts, "false")
	got := captureConflictsIn(captureExporter(), exporter{}.layer())
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
	t.Setenv(otelLogUserPrompts, "1")
	if got := captureConflictsIn(captureExporter(), exporter{}.layer()); len(got) != 0 {
		t.Fatalf("expected no conflict when the export agrees, got %+v", got)
	}
}

// Without content capture requested there is nothing to override, so no noise.
func TestCaptureConflictsSilentWhenNotCapturing(t *testing.T) {
	t.Setenv(otelLogUserPrompts, "0")
	e := captureExporter()
	e.IncludePrompts = false
	for _, c := range captureConflictsIn(e, exporter{}.layer()) {
		if c.Key == otelLogUserPrompts {
			t.Fatalf("reported an override for content Terma is not capturing: %+v", c)
		}
	}
}

// A project settings file that turns capture off outranks the user file terma writes.
func TestCaptureConflictsReportsProjectOverride(t *testing.T) {
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

	got := captureConflictsIn(captureExporter(), exporter{}.layer())
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
	t.Setenv(otelLogUserPrompts, "0")
	t.Setenv(otelLogToolContent, "0")
	for _, c := range captureConflictsIn(captureExporter(), exporter{}.layer()) {
		if !c.Advisory {
			t.Fatalf("%s would block a connect", c.Key)
		}
	}
}

// render writes every capture key, so a previous exclusion never leaks into the next connect.
func TestRenderAlwaysStatesCapturePosture(t *testing.T) {
	on := exporter{}.render(captureExporter())
	e := captureExporter()
	e.IncludePrompts, e.IncludeToolContent = false, false
	off := exporter{}.render(e)
	for _, key := range captureKeys {
		if on[key] != "1" {
			t.Errorf("capture on: %s = %q, want \"1\"", key, on[key])
		}
		if off[key] != "0" {
			t.Errorf("capture off: %s = %q, want \"0\"", key, off[key])
		}
	}
}
