package e2e

import (
	"slices"
	"testing"
)

// The nightly notes a field the relay has not classified instead of failing the scenario: a
// harness that sends one still sends all the telemetry terma expects.
func TestUnclassifiedIsANoteInTheNightly(t *testing.T) {
	t.Setenv("TERMA_E2E_UNCLASSIFIED", "note")
	failUnclassified(t, map[string]int{"unclassified.permission_mode": 2, "forwarded.logs": 3})
	reportMu.Lock()
	notes := slices.Clone(reportNotes["relay/unclassified"])
	delete(reportNotes, "relay/unclassified")
	reportMu.Unlock()
	if !slices.Contains(notes, "permission_mode") {
		t.Errorf("notes %q: the unclassified key is not noted", notes)
	}
}
