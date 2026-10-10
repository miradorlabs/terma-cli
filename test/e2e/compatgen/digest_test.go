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

// A night that brought no census, or none of a harness the catalog censused within the week,
// is no quiet night: a census that did not run must not read as one that found nothing.
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

// A surface no build had is said once, with its keys counted, not key by key: a span renamed
// carries keys the harness already sends. A key new to the harness on it is still a new
// field. A re-run of the same build reports nothing gone: a key that only comes on an error
// path would otherwise read as removed.
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

// A night that reached only a build older than the catalog's newest says so, and is no quiet
// night: the newest failed to install, or its tests did not run.
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

// A night whose census scenarios ran a newer build than any census reached says so, though
// the build the census did reach is newer than the catalog's: the newest failed before its
// census was taken.
func TestDigestSaysTheNewestRunWasNotReached(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.160.0", "logs/codex.api_request", "model", "safe", day)}, nil)
	night := day.Add(24 * time.Hour)
	ran := censusRan([]e2e.CensusRun{
		{Harness: "codex", Version: "0.161.0"},
		{Harness: "codex", Version: "0.162.0", Failed: true},
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

// A census a scenario failed before taking whole judges nothing removed: what it did not see
// it may not have reached. A census every scenario took whole does.
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
		ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.162.0", Failed: c.result == "fail"}})
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

// What a harness names is escaped where it is shown: "<" in Slack would start a link, and "|"
// in a markdown table would end a cell.
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

// A harness whose census scenarios ran and took no census is missing, though the catalog
// never censused it: one that fails before its census every night is no quiet night.
func TestDigestSaysAnUncensusedHarnessThatRanIsMissing(t *testing.T) {
	now := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	ran := censusRan([]e2e.CensusRun{
		{Harness: "gemini", Version: "0.64.0", Failed: true},
		{Harness: "codex", Version: "0.162.0"},
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
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.170.0"}})
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
	failed := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.162.0", Failed: true}})
	if _, _, hs := fieldDrift(cat, tonight, failed, day.Add(24*time.Hour)); !hs[0].Partial || len(hs[0].Removed) != 0 {
		t.Fatalf("partial night: %+v", hs[0])
	}
	mergeFields(&cat, tonight, failed.failed)
	passed := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.162.0"}})
	if _, _, hs := fieldDrift(cat, tonight, passed, day.Add(48*time.Hour)); len(hs[0].Removed) != 1 {
		t.Errorf("whole night: removed %+v, want retry_reason", hs[0].Removed)
	}
}

// A key sent by the partial builds after the last whole census, and by more of them than a key
// keeps builds of, is still judged gone when a whole build no longer sends it; and the digest
// names the build it was judged against.
func TestARemovalAcrossPartialBuilds(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	both := func(v string, at time.Time) []e2e.FieldRow {
		return []e2e.FieldRow{field(v, "logs/codex.api_request", "model", "safe", at), field(v, "logs/codex.api_request", "attempt", "safe", at)}
	}
	var cat Catalog
	mergeFields(&cat, both("0.150.0", night(0)), nil)
	for i := 1; i <= keepVersions; i++ {
		v := fmt.Sprintf("0.15%d.0", i)
		mergeFields(&cat, both(v, night(i)), map[string]bool{"codex\x00" + v: true})
	}
	tonight := []e2e.FieldRow{field("0.160.0", "logs/codex.api_request", "model", "safe", night(9))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.160.0"}})
	_, _, hs := fieldDrift(cat, tonight, ran, night(9))
	h := hs[0]
	if len(h.Removed) != 1 || h.Removed[0].Key != "attempt" {
		t.Errorf("removed %+v, want attempt", h.Removed)
	}
	if h.Previous != "0.155.0" || h.Since != "0.150.0" || !strings.Contains(h.headline(), "(was 0.155.0; judged against 0.150.0, the last whole census)") {
		t.Errorf("previous %s, since %s, headline %q", h.Previous, h.Since, h.headline())
	}
}

// A key only a failed run saw, an error path's, is no evidence the build before sent it: a
// whole build without it reports nothing removed.
func TestAKeyOnlyAPartialRunSawIsNotRemoved(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.150.0", "logs/codex.api_request", "model", "safe", day)}, nil)
	mergeFields(&cat, []e2e.FieldRow{
		field("0.151.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.151.0", "logs/codex.api_request", "error.message", "prompt", day.Add(24*time.Hour)),
	}, map[string]bool{"codex\x000.151.0": true})
	tonight := []e2e.FieldRow{field("0.152.0", "logs/codex.api_request", "model", "safe", day.Add(48*time.Hour))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.152.0"}})
	if _, _, hs := fieldDrift(cat, tonight, ran, day.Add(48*time.Hour)); len(hs[0].Removed) != 0 {
		t.Errorf("removed %+v: only a failed run sent it", hs[0].Removed)
	}
}

// Ten partial builds in a row, the first five seeing a key and the rest not, prune neither
// the key nor the whole census that saw it: the whole build after them reports it removed.
func TestTenPartialBuildsKeepAKeyTheWholeCensusSaw(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.150.0", "logs/codex.api_request", "model", "safe", night(0)),
		field("0.150.0", "logs/codex.api_request", "attempt", "safe", night(0))}, nil)
	for i := 1; i <= 2*keepCensuses; i++ {
		v := fmt.Sprintf("0.1%d.0", 50+i)
		rows := []e2e.FieldRow{field(v, "logs/codex.api_request", "model", "safe", night(i))}
		if i <= keepCensuses {
			rows = append(rows, field(v, "logs/codex.api_request", "attempt", "safe", night(i)))
		}
		mergeFields(&cat, rows, map[string]bool{"codex\x00" + v: true})
	}
	if !slices.ContainsFunc(cat.Fields, func(f FieldEntry) bool { return f.Key == "attempt" && slices.Contains(f.Whole, "0.150.0") }) {
		t.Fatalf("attempt, or its whole build, pruned: %+v", cat.Fields)
	}
	tonight := []e2e.FieldRow{field("0.170.0", "logs/codex.api_request", "model", "safe", night(20))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.170.0"}})
	if _, _, hs := fieldDrift(cat, tonight, ran, night(20)); len(hs[0].Removed) != 1 || hs[0].Removed[0].Key != "attempt" {
		t.Errorf("removed %+v, want attempt", hs[0].Removed)
	}
}

// A build judged against an older one than the build before says so, on every line that
// names it.
func TestTheBuildJudgedAgainstIsNamed(t *testing.T) {
	h := HarnessDrift{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.162.0", Since: "0.161.0",
		Removed: []FieldChange{{Surface: "logs/x", Key: "k"}}}
	if got := h.headline(); !strings.HasPrefix(got, "Codex CLI 0.162.0 (judged against 0.161.0, the last whole census): 1 removed field") {
		t.Errorf("headline %q", got)
	}
	h.Removed, h.Previous, h.Compare = nil, "0.162.0", "https://c"
	h.Version = "0.162.1"
	if got := (Drift{Harnesses: []HarnessDrift{h}}).unchanged(); got != "Codex CLI 0.162.1 (new build, was 0.162.0; judged against 0.161.0, the last whole census, \x00https://c\x01diff\x02)" {
		t.Errorf("unchanged %q", got)
	}
}

// The nightly job censuses each build about three nights running: a build censused partially
// one night and whole another, in either order, takes nothing from the failed night as
// evidence. A key and a surface only the failed run sent are not reported gone from the next.
func TestAWholeBuildTakesNothingFromItsFailedNight(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	failedRun := []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", night(1)),
		field("0.163.0", "logs/codex.api_request", "error.message", "prompt", night(1)),
		field("0.163.0", "logs/codex.stream_error", "reason", "safe", night(1))}
	wholeRun := []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", night(2))}
	failed := map[string]bool{"codex\x000.163.0": true}
	for _, order := range []string{"failed first", "whole first"} {
		var cat Catalog
		mergeFields(&cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", night(0))}, nil)
		if order == "failed first" {
			mergeFields(&cat, failedRun, failed)
			mergeFields(&cat, wholeRun, nil)
		} else {
			mergeFields(&cat, wholeRun, nil)
			mergeFields(&cat, failedRun, failed)
		}
		tonight := []e2e.FieldRow{field("0.164.0", "logs/codex.api_request", "model", "safe", night(3))}
		ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0"}})
		_, _, hs := fieldDrift(cat, tonight, ran, night(3))
		if h := hs[0]; len(h.Removed)+len(h.Unseen) != 0 {
			t.Errorf("%s: removed %+v, unseen %+v: only the failed run sent them", order, h.Removed, h.Unseen)
		}
	}
}

// A night that censuses two new builds, the older whole and the newer partial, judges the
// whole one, which would otherwise become the next build's baseline unjudged: a key it no
// longer sends is reported that night, and not again when the newer build is censused whole.
func TestTheNewestWholeBuildOfANightIsJudged(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", night(0)),
		field("0.163.0", "logs/codex.api_request", "attempt", "safe", night(0))}, nil)
	tonight := []e2e.FieldRow{field("0.164.0", "logs/codex.api_request", "model", "safe", night(1)),
		field("0.165.0", "logs/codex.api_request", "model", "safe", night(1))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0"}, {Harness: "codex", Version: "0.165.0", Failed: true}})
	_, _, hs := fieldDrift(cat, tonight, ran, night(1))
	h := hs[0]
	if h.Version != "0.165.0" || h.Judged != "0.164.0" || len(h.Removed) != 1 || h.Removed[0].Key != "attempt" {
		t.Fatalf("version %s, judged %s, removed %+v", h.Version, h.Judged, h.Removed)
	}
	if !strings.Contains(h.headline(), "census partial (a scenario failed), removals judged on 0.164.0") {
		t.Errorf("headline %q", h.headline())
	}
	mergeFields(&cat, tonight, ran.failed)
	next := []e2e.FieldRow{field("0.165.0", "logs/codex.api_request", "model", "safe", night(2))}
	ran = censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.165.0"}})
	if _, _, hs = fieldDrift(cat, next, ran, night(2)); len(hs[0].Removed) != 0 || hs[0].Since != "" && hs[0].Since != "0.164.0" {
		t.Errorf("the next night: removed %+v, judged against %s", hs[0].Removed, hs[0].Since)
	}
}

// What the build judged added is said the night it is judged, though the newer build's
// partial census missed it: a key it adds, withheld, and a surface it renames to. The next
// night, the newer build censused whole says none of it again.
func TestWhatTheJudgedBuildAddedIsNew(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", night(0)),
		field("0.163.0", "logs/codex.old_event", "model", "safe", night(0))}, nil)
	tonight := []e2e.FieldRow{
		field("0.164.0", "logs/codex.api_request", "model", "safe", night(1)),
		field("0.164.0", "logs/codex.api_request", "newkey", "unclassified", night(1)),
		field("0.164.0", "logs/codex.new_event", "model", "safe", night(1)),
		field("0.165.0", "logs/codex.api_request", "model", "safe", night(1)),
	}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0"}, {Harness: "codex", Version: "0.165.0", Failed: true}})
	_, _, hs := fieldDrift(cat, tonight, ran, night(1))
	h := hs[0]
	keys := func(cs []FieldChange) (out []string) {
		for _, c := range cs {
			out = append(out, c.Key)
		}
		return out
	}
	if !slices.Equal(keys(h.Added), []string{"newkey"}) || !slices.Equal(keys(h.Withheld), []string{"newkey"}) ||
		len(h.NewSurfaces) != 1 || h.NewSurfaces[0].Surface != "logs/codex.new_event" || len(h.Unseen) != 1 {
		t.Fatalf("added %v, withheld %v, new surfaces %+v, unseen %+v", keys(h.Added), keys(h.Withheld), h.NewSurfaces, h.Unseen)
	}
	mergeFields(&cat, tonight, ran.failed)
	next := []e2e.FieldRow{field("0.165.0", "logs/codex.api_request", "model", "safe", night(2)),
		field("0.165.0", "logs/codex.api_request", "newkey", "unclassified", night(2)),
		field("0.165.0", "logs/codex.new_event", "model", "safe", night(2))}
	_, _, hs = fieldDrift(cat, next, censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.165.0"}}), night(2))
	if h := hs[0]; len(h.Added)+len(h.Withheld)+len(h.NewSurfaces)+len(h.Removed)+len(h.Unseen) != 0 {
		t.Errorf("the next night said it again: %+v", h)
	}
}
