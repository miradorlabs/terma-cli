package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

func TestDigestSaysWhatWasNotCensused(t *testing.T) {
	now := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	cat := Catalog{Censuses: []Census{
		{Harness: "codex", Version: "0.162.0", At: now.Add(-24 * time.Hour)},
		{Harness: "hermes", Version: "0.20.5", At: now.Add(-30 * 24 * time.Hour)}, // aged out
	}}
	noCensus, missing, _ := fieldDrift(cat, nil, censusRuns{}, now)
	if d := (Drift{NoCensus: noCensus, Missing: missing}); !noCensus || d.Quiet() {
		t.Errorf("no rows: noCensus %v, quiet %v", noCensus, d.Quiet())
	}
	rows := []e2e.FieldRow{field("2.1.296", "logs/api_request", "model", "safe", now)}
	rows[0].Harness = "claude"
	noCensus, missing, hs := fieldDrift(cat, rows, censusRuns{}, now)
	d := Drift{NoCensus: noCensus, Missing: missing, Harnesses: hs}
	if noCensus || !slices.Equal(missing, []string{"Codex CLI"}) || d.Quiet() {
		t.Errorf("claude only: noCensus %v, missing %v, quiet %v", noCensus, missing, d.Quiet())
	}
	if !strings.Contains(d.markdown(), "No census this night of Codex CLI") {
		t.Errorf("the digest does not say Codex was not censused:\n%s", d.markdown())
	}
}

func TestDigestSurfacesAndReruns(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.162.0", "traces/old_fn", "code.file.path", "safe", day),
		field("0.162.0", "traces/old_fn", "busy_ns", "safe", day, e2e.KindNumber),
		field("0.162.0", "logs/codex.tool_result", "error", "prompt", day), // only on an error path
	}, nil)
	tonight := []e2e.FieldRow{
		field("0.162.0", "traces/new_fn", "code.file.path", "safe", day.Add(24*time.Hour)),
		field("0.162.0", "traces/new_fn", "busy_ns", "safe", day.Add(24*time.Hour), e2e.KindNumber),
		field("0.162.0", "traces/new_fn", "fresh_key", "safe", day.Add(24*time.Hour)),
	}
	_, _, hs := fieldDrift(cat, tonight, censusRuns{}, day.Add(24*time.Hour))
	h := hs[0]
	if len(h.NewSurfaces) != 1 || h.NewSurfaces[0] != (SurfaceChange{Surface: "traces/new_fn", Keys: 3, NewKeys: 1}) {
		t.Errorf("new surfaces %+v", h.NewSurfaces)
	}
	if len(h.Added) != 1 || h.Added[0].Key != "fresh_key" {
		t.Errorf("added %+v: only the key new to the harness", h.Added)
	}
	if len(h.Removed)+len(h.Unseen) != 0 {
		t.Errorf("a re-run of the same build reported removed %+v, unseen %v", h.Removed, h.Unseen)
	}
}

func TestDigestSaysTheNewestWasNotReached(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.162.1", "logs/codex.api_request", "model", "safe", day)}, nil)
	_, _, hs := fieldDrift(cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour))}, censusRuns{}, day.Add(24*time.Hour))
	d := Drift{Harnesses: hs}
	if hs[0].Unreached != "0.162.1" || d.Quiet() || !strings.Contains(hs[0].headline(), "0.162.1, the newest censused before, was not reached") {
		t.Errorf("unreached %q, quiet %v, headline %q", hs[0].Unreached, d.Quiet(), hs[0].headline())
	}
}

func TestDigestSaysTheNewestRunWasNotReached(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.160.0", "logs/codex.api_request", "model", "safe", day)}, nil)
	night := day.Add(24 * time.Hour)
	ran := censusRan([]e2e.CompatRow{
		{Harness: "codex", Version: "0.161.0", Capability: e2e.CensusCapability, Result: "pass"},
		{Harness: "codex", Version: "0.162.0", Capability: e2e.CensusCapability, Result: "fail"},
		// Neither a skipped scenario nor another capability's is a census that should have been taken.
		{Harness: "codex", Version: "0.163.0", Capability: e2e.CensusCapability, Result: "not run"},
		{Harness: "codex", Version: "0.164.0", Capability: "relay.telemetry", Result: "fail"},
	})
	_, _, hs := fieldDrift(cat, []e2e.FieldRow{field("0.161.0", "logs/codex.api_request", "model", "safe", night)}, ran, night)
	d := Drift{Harnesses: hs}
	if hs[0].Unreached != "0.162.0" || d.Quiet() || !strings.Contains(hs[0].headline(), "0.161.0 (0.162.0, run tonight, was not reached)") {
		t.Errorf("unreached %q, quiet %v, headline %q", hs[0].Unreached, d.Quiet(), hs[0].headline())
	}
	// The census of the newest build it ran: nothing unreached, but a scenario of it failed,
	// so its census is partial.
	_, _, hs = fieldDrift(cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", night)}, ran, night)
	if hs[0].Unreached != "" || !hs[0].Partial {
		t.Errorf("unreached %q, partial %v with the newest censused", hs[0].Unreached, hs[0].Partial)
	}
}

func TestDigestJudgesNoRemovalFromAPartialCensus(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.161.0", "logs/codex.api_request", "model", "safe", day),
		field("0.161.0", "logs/codex.api_request", "attempt", "safe", day),
		field("0.161.0", "logs/codex.tool_result", "tool_name", "safe", day),
	}, nil)
	night := day.Add(24 * time.Hour)
	tonight := []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", night)}
	for _, c := range []struct {
		result  string
		removed int
	}{{"fail", 0}, {"pass", 1}} {
		ran := censusRan([]e2e.CompatRow{{Harness: "codex", Version: "0.162.0", Capability: e2e.CensusCapability, Result: c.result}})
		_, _, hs := fieldDrift(cat, tonight, ran, night)
		h := hs[0]
		if h.Partial != (c.result == "fail") || len(h.Removed) != c.removed || len(h.Unseen) != c.removed {
			t.Errorf("%s: partial %v, removed %v, unseen %v", c.result, h.Partial, h.Removed, h.Unseen)
		}
		if c.result == "fail" && (!strings.Contains(h.headline(), "census partial (a scenario failed), nothing judged removed") || (Drift{Harnesses: hs}).Quiet()) {
			t.Errorf("a partial census: %q", h.headline())
		}
	}
}

func TestDigestEscapesWhatHarnessesName(t *testing.T) {
	d := Drift{GeneratedAt: time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC), Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "1", Previous: "1",
		Added: []FieldChange{{Surface: "logs/<x|y>", Key: "a&b"}}}}}
	data, _ := json.Marshal(d.slack())
	if !strings.Contains(string(data), "a\\u0026amp;b") || !strings.Contains(string(data), "\\u0026lt;x|y\\u0026gt;") {
		t.Errorf("slack text not escaped: %s", data)
	}
	if got := tableCell("a|b"); got != `a\|b` {
		t.Errorf("tableCell = %q", got)
	}
}

func TestDigestSaysAnUncensusedHarnessThatRanIsMissing(t *testing.T) {
	now := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	ran := censusRan([]e2e.CompatRow{
		{Harness: "gemini", Version: "0.64.0", Capability: e2e.CensusCapability, Result: "fail"},
		{Harness: "codex", Version: "0.162.0", Capability: e2e.CensusCapability, Result: "pass"},
	})
	noCensus, missing, _ := fieldDrift(cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", now)}, ran, now)
	d := Drift{NoCensus: noCensus, Missing: missing}
	if !slices.Equal(missing, []string{"Gemini CLI"}) || d.Quiet() {
		t.Errorf("missing %v, quiet %v", missing, d.Quiet())
	}
}

// Partial censuses neither push the last whole one out of the catalog nor hide a removal: a
// key the last whole census saw stays through five partial builds, and the next whole build
// without it reports it removed, once.
func TestPartialCensusesKeepTheWholeOne(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.160.0", "logs/codex.api_request", "model", "safe", day),
		field("0.160.0", "logs/codex.api_request", "retry_reason", "safe", day),
	}, nil)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	for i := 1; i <= keepCensuses; i++ {
		v := fmt.Sprintf("0.16%d.0", i)
		partial := map[string]bool{"codex\x00" + v: true}
		mergeFields(&cat, []e2e.FieldRow{field(v, "logs/codex.api_request", "model", "safe", night(i))}, partial)
	}
	if !slices.ContainsFunc(cat.Fields, func(f FieldEntry) bool { return f.Key == "retry_reason" }) {
		t.Fatal("partial censuses pruned a key the last whole census saw")
	}
	if whole, ok := cat.lastWhole("codex", "0.170.0"); !ok || whole.Version != "0.160.0" {
		t.Fatalf("last whole census %+v, %v", whole, ok)
	}
	tonight := []e2e.FieldRow{field("0.170.0", "logs/codex.api_request", "model", "safe", night(9))}
	ran := censusRan([]e2e.CompatRow{{Harness: "codex", Version: "0.170.0", Capability: e2e.CensusCapability, Result: "pass"}})
	_, _, hs := fieldDrift(cat, tonight, ran, night(9))
	if len(hs[0].Removed) != 1 || hs[0].Removed[0].Key != "retry_reason" {
		t.Errorf("removed %+v, want retry_reason, judged against the last whole census", hs[0].Removed)
	}
	mergeFields(&cat, tonight, ran.failed)
	if _, _, hs = fieldDrift(cat, tonight, ran, night(10)); len(hs[0].Removed) != 0 {
		t.Errorf("a re-run of a build censused whole reported %+v removed again", hs[0].Removed)
	}
}

// A build censused partially one night and whole the next is judged on the night it is whole.
func TestAPartialBuildIsJudgedWhenWhole(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.161.0", "logs/codex.api_request", "model", "safe", day),
		field("0.161.0", "logs/codex.api_request", "retry_reason", "safe", day),
	}, nil)
	tonight := []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour))}
	failed := censusRan([]e2e.CompatRow{{Harness: "codex", Version: "0.162.0", Capability: e2e.CensusCapability, Result: "fail"}})
	if _, _, hs := fieldDrift(cat, tonight, failed, day.Add(24*time.Hour)); !hs[0].Partial || len(hs[0].Removed) != 0 {
		t.Fatalf("partial night: %+v", hs[0])
	}
	mergeFields(&cat, tonight, failed.failed)
	passed := censusRan([]e2e.CompatRow{{Harness: "codex", Version: "0.162.0", Capability: e2e.CensusCapability, Result: "pass"}})
	if _, _, hs := fieldDrift(cat, tonight, passed, day.Add(48*time.Hour)); len(hs[0].Removed) != 1 {
		t.Errorf("whole night: removed %+v, want retry_reason", hs[0].Removed)
	}
}
