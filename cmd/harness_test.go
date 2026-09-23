package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// runHarness is a run that must succeed, read from stdout alone: these tests parse
// what `harness list` prints, and a warning on stderr is not part of it.
func runHarness(t *testing.T, args ...string) string {
	t.Helper()
	stdout, _, err := termaRun{}.exec(t, args...)
	if err != nil {
		t.Fatalf("terma %s: %v", strings.Join(args, " "), err)
	}
	return stdout
}

// The table view is the human answer to "which harnesses are supported": every agent
// appears, and Cursor's missing telemetry reads as partial with a note.
func TestHarnessListTable(t *testing.T) {
	out := runHarness(t, "harness", "list", "-o", "table")
	for _, want := range []string{"Claude Code", "Codex", "OpenCode", "Cursor", "ATTRIBUTION", "TELEMETRY", "SUPPORT"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "partial") {
		t.Errorf("no partial support shown for an attribution-only harness:\n%s", out)
	}
	if !strings.Contains(out, "billed cost unavailable") {
		t.Errorf("Cursor's telemetry gap is not explained:\n%s", out)
	}
}

// A bare `terma harness` defaults to the support catalog rather than printing help.
func TestHarnessBareDefaultsToList(t *testing.T) {
	out := runHarness(t, "harness", "-o", "table")
	if !strings.Contains(out, "Cursor") || !strings.Contains(out, "SUPPORT") {
		t.Errorf("bare `terma harness` did not show the support catalog:\n%s", out)
	}
}

// JSON carries the full per-capability structure, including levels the table flattens
// to a word, so a script can act on the gap.
func TestHarnessListJSON(t *testing.T) {
	out := runHarness(t, "harness", "list", "-o", "json")
	var report struct {
		Harnesses []harness.AgentSupport `json:"harnesses"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse json: %v\n%s", err, out)
	}
	byName := map[string]harness.AgentSupport{}
	for _, a := range report.Harnesses {
		byName[a.Name] = a
	}
	cursor, ok := byName["cursor"]
	if !ok {
		t.Fatalf("cursor missing from json:\n%s", out)
	}
	if cursor.Support != harness.SupportPartial {
		t.Errorf("cursor support = %q, want partial", cursor.Support)
	}
	if cursor.Telemetry.Level != harness.SupportPartial {
		t.Errorf("cursor telemetry = %q, want partial", cursor.Telemetry.Level)
	}
	if claude := byName["claude"]; claude.Support != harness.SupportFull {
		t.Errorf("claude support = %q, want full", claude.Support)
	}
}

// A single-harness argument narrows the catalog to that one row.
func TestHarnessListSingle(t *testing.T) {
	out := runHarness(t, "harness", "list", "cursor", "-o", "table")
	if !strings.Contains(out, "Cursor") {
		t.Errorf("cursor row missing:\n%s", out)
	}
	if strings.Contains(out, "Claude Code") {
		t.Errorf("asked for cursor only but claude appeared:\n%s", out)
	}
}

func TestHarnessListUnknown(t *testing.T) {
	_, err := runTerma(t, "harness", "list", "gemini")
	if err == nil {
		t.Fatal("expected an error for an unknown harness")
	}
	if !strings.Contains(err.Error(), "gemini") {
		t.Errorf("error did not name the unknown harness: %v", err)
	}
	// "Agent" is the word a developer sees everywhere else; harness and adapter are
	// terma's own names for the two registries.
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("error should call it an agent: %v", err)
	}
}
