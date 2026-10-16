package e2e

import (
	"slices"
	"testing"
)

// The nightly notes a field the relay has not classified instead of failing the scenario: a
// harness that sends one still sends all the telemetry terma expects.
func TestUnclassifiedIsANoteInTheNightly(t *testing.T) {
	t.Setenv("TERMA_E2E_UNCLASSIFIED", "note")
	// A key no harness sends, and the notes as they were put back: in a nightly this runs
	// after the scenarios, whose own notes the report must keep.
	const key = "terma_e2e_self_test_key"
	reportMu.Lock()
	before, had := reportNotes["relay/unclassified"]
	before = slices.Clone(before)
	reportMu.Unlock()
	t.Cleanup(func() {
		reportMu.Lock()
		defer reportMu.Unlock()
		if had {
			reportNotes["relay/unclassified"] = before
		} else {
			delete(reportNotes, "relay/unclassified")
		}
	})
	failUnclassified(t, map[string]int{"unclassified." + key: 2, "forwarded.logs": 3})
	reportMu.Lock()
	notes := slices.Clone(reportNotes["relay/unclassified"])
	reportMu.Unlock()
	if !slices.Contains(notes, key) {
		t.Errorf("notes %q: the unclassified key is not noted", notes)
	}
}
