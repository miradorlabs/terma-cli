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
	before, had := reportNotes[UnclassifiedNotes]
	before = slices.Clone(before)
	reportMu.Unlock()
	t.Cleanup(func() {
		reportMu.Lock()
		defer reportMu.Unlock()
		if had {
			reportNotes[UnclassifiedNotes] = before
		} else {
			delete(reportNotes, UnclassifiedNotes)
		}
	})
	failUnclassified(t, map[string]int{"unclassified." + key: 2, "forwarded.logs": 3})
	reportMu.Lock()
	notes := slices.Clone(reportNotes[UnclassifiedNotes])
	reportMu.Unlock()
	if !slices.Contains(notes, UnclassifiedNote("", key)) {
		t.Errorf("notes %q: the unclassified key is not noted", notes)
	}
}

// A key is noted with the harness the scenario that met it proves, found up its parents.
func TestUnclassifiedNotesNameTheHarness(t *testing.T) {
	compatMu.Lock()
	provesHarness[t.Name()+"/v1.0.0"] = "codex"
	compatMu.Unlock()
	t.Cleanup(func() {
		compatMu.Lock()
		delete(provesHarness, t.Name()+"/v1.0.0")
		compatMu.Unlock()
	})
	t.Run("v1.0.0", func(t *testing.T) {
		t.Run("withheld", func(t *testing.T) {
			if h := harnessOf(t); h != "codex" {
				t.Errorf("harness %q, want codex", h)
			}
		})
	})
	if h := harnessOf(t); h != "" {
		t.Errorf("harness %q above the scenario, want none", h)
	}
	for _, n := range []string{UnclassifiedNote("codex", "state"), UnclassifiedNote("", "resource/host.id")} {
		h, k := ParseUnclassifiedNote(n)
		if UnclassifiedNote(h, k) != n {
			t.Errorf("%q parses to %q, %q", n, h, k)
		}
	}
}
