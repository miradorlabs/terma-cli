package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func captureExporter() Exporter {
	return Exporter{
		Endpoint:           "https://otel.terma.ai",
		APIKey:             "mir_srv_test",
		Signals:            AllSignals,
		IncludePrompts:     true,
		IncludeToolContent: true,
	}
}

func findConflict(conflicts []Conflict, key string) *Conflict {
	for i := range conflicts {
		if conflicts[i].Key == key {
			return &conflicts[i]
		}
	}
	return nil
}

// A shell export of a capture key as off beats the settings file, so a connect that
// means to capture must say the content will not arrive.
func TestCaptureConflictsReportsShellOverride(t *testing.T) {
	t.Setenv(otelLogUserPrompts, "false")
	got := captureConflicts(captureExporter())
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
	if c.Scope != ScopeEnvironment {
		t.Errorf("scope = %q, want %q", c.Scope, ScopeEnvironment)
	}
}

// The same key exported as on is not a conflict — it agrees with the intent.
func TestCaptureConflictsIgnoresAgreeingValue(t *testing.T) {
	t.Setenv(otelLogUserPrompts, "1")
	if got := captureConflicts(captureExporter()); len(got) != 0 {
		t.Fatalf("expected no conflict when the export agrees, got %+v", got)
	}
}

// Nothing is being overridden if Terma is not asking for the content in the first
// place, so --exclude-prompts must not produce noise.
func TestCaptureConflictsSilentWhenNotCapturing(t *testing.T) {
	t.Setenv(otelLogUserPrompts, "0")
	e := captureExporter()
	e.IncludePrompts = false
	for _, c := range captureConflicts(e) {
		if c.Key == otelLogUserPrompts {
			t.Fatalf("reported an override for content Terma is not capturing: %+v", c)
		}
	}
}

// A project settings file outranks the user file Terma writes.
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

	got := captureConflicts(captureExporter())
	c := findConflict(got, otelLogToolContent)
	if c == nil {
		t.Fatalf("expected %s to be reported, got %+v", otelLogToolContent, got)
	}
	if c.Scope != ScopeProject || !c.Advisory {
		t.Errorf("got scope=%q advisory=%v, want project/advisory", c.Scope, c.Advisory)
	}
}

// A capture override must never gate a connect: it is reported and nothing more.
func TestCaptureConflictsNeverBlock(t *testing.T) {
	t.Setenv(otelLogUserPrompts, "0")
	t.Setenv(otelLogToolContent, "0")
	for _, c := range captureConflicts(captureExporter()) {
		if !c.Advisory {
			t.Fatalf("%s would block a connect", c.Key)
		}
	}
}

// A previous connect that excluded content must not leak into the next one: Render
// writes every capture key explicitly, so the flags alone decide.
func TestRenderAlwaysStatesCapturePosture(t *testing.T) {
	on := Claude{}.render(captureExporter())
	e := captureExporter()
	e.IncludePrompts, e.IncludeToolContent = false, false
	off := Claude{}.render(e)
	for _, key := range captureKeys {
		if on[key] != "1" {
			t.Errorf("capture on: %s = %q, want \"1\"", key, on[key])
		}
		if off[key] != "0" {
			t.Errorf("capture off: %s = %q, want \"0\"", key, off[key])
		}
	}
}
