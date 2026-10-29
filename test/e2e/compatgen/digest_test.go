package main

import (
	"encoding/json"
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
	if hs[0].Unreached != "0.162.0" || d.Quiet() || !strings.Contains(hs[0].headline(), "0.161.0 (0.162.0, run tonight, was not reached; judged against 0.160.0's earlier nights, not censused tonight)") {
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

// A build judged against an older one than the build before says so, on every line that
// names it.
func TestTheBuildJudgedAgainstIsNamed(t *testing.T) {
	h := HarnessDrift{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.162.0", Since: "0.161.0",
		Removed: []FieldChange{{Surface: "logs/x", Key: "k"}}}
	if got := h.headline(); !strings.HasPrefix(got, "Codex CLI 0.162.0 (judged against 0.161.0, censused whole): 1 removed field") {
		t.Errorf("headline %q", got)
	}
	h.Removed, h.Previous, h.Compare = nil, "0.162.0", "https://c"
	h.Version = "0.162.1"
	if got := h.build("new build, "); got != "0.162.1 (new build, was 0.162.0; judged against 0.161.0, censused whole, \x00https://c\x01diff\x02)" {
		t.Errorf("build %q", got)
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

// What any build the catalog has no census of brings is new the night it comes, whole or
// partial, judged or not: it all goes into the catalog that night.
func TestWhatEveryNewBuildBringsIsNew(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", day)}, nil)
	tonight := []e2e.FieldRow{
		field("0.164.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.164.0", "logs/codex.api_request", "from_partial", "safe", day.Add(24*time.Hour)),
		field("0.165.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.165.0", "logs/codex.api_request", "from_whole", "safe", day.Add(24*time.Hour)),
		field("0.166.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
	}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0", Failed: true}, {Harness: "codex", Version: "0.165.0"},
		{Harness: "codex", Version: "0.166.0", Failed: true}})
	_, _, hs := fieldDrift(cat, tonight, ran, day.Add(24*time.Hour))
	var added []string
	for _, c := range hs[0].Added {
		added = append(added, c.Key)
	}
	if !slices.Equal(added, []string{"from_partial", "from_whole"}) {
		t.Errorf("added %v", added)
	}
}

// Two new builds censused whole in one night are each judged, oldest first, against the one
// before: a key the first drops is gone in it, and one the first adds and the second drops
// is new and gone. The next night says none of it again.
func TestEachNewWholeBuildOfANightIsJudgedInTurn(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", night(0)),
		field("0.163.0", "logs/codex.api_request", "dropped_early", "safe", night(0))}, nil)
	tonight := []e2e.FieldRow{
		field("0.164.0", "logs/codex.api_request", "model", "safe", night(1)),
		field("0.164.0", "logs/codex.api_request", "brief", "safe", night(1)),
		field("0.165.0", "logs/codex.api_request", "model", "safe", night(1)),
	}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0"}, {Harness: "codex", Version: "0.165.0"}})
	_, _, hs := fieldDrift(cat, tonight, ran, night(1))
	h := hs[0]
	var added, removed []string
	for _, c := range h.Added {
		added = append(added, c.Key)
	}
	for _, c := range h.Removed {
		removed = append(removed, c.Key+"@"+c.In)
	}
	if !slices.Equal(added, []string{"brief"}) || !slices.Equal(removed, []string{"dropped_early@0.164.0", "brief@"}) {
		t.Fatalf("added %v, removed %v", added, removed)
	}
	if md := (Drift{Harnesses: hs}).markdown(); !strings.Contains(md, "🔴 `-dropped_early` on `logs/codex.api_request` (safe) (gone in 0.164.0)") {
		t.Errorf("drift.md does not say where it went:\n%s", md)
	}
	mergeFields(&cat, tonight, ran.failed)
	next := []e2e.FieldRow{field("0.165.0", "logs/codex.api_request", "model", "safe", night(2))}
	if _, _, hs = fieldDrift(cat, next, censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.165.0"}}), night(2)); hs[0].changed() {
		t.Errorf("the next night said it again: %+v", hs[0])
	}
}

// A first census a scenario failed for says it is partial: its counts are not the harness's.
func TestAPartialFirstCensusSaysSo(t *testing.T) {
	h := HarnessDrift{Name: "Pi", Version: "0.84.2", First: true, Partial: true, Surfaces: 2, Keys: 9}
	if got := h.headline(); got != "Pi 0.84.2: first census, 2 surfaces, 9 keys, census partial (a scenario failed)" {
		t.Errorf("headline %q", got)
	}
}

// A span event the new build sends on another span, with the same keys, moved: Codex 0.165.0
// emits user_prompt inside run_hooks_and_record_inputs, not codex.turn_input. It is said once,
// unsigned, not as a surface gone and another new, and the release counts it.
func TestASpanEventOnAnotherSpanMoved(t *testing.T) {
	day := time.Date(2026, 10, 26, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	// A key the build before the baseline sent is in the catalog, not in what is judged.
	mergeFields(&cat, promptRows("0.163.0", day, "codex.turn_input:prompt_length,slug,dropped_key"), nil)
	mergeFields(&cat, promptRows("0.164.0", day.Add(24*time.Hour), "codex.turn_input"), nil)
	tonight := day.Add(48 * time.Hour)
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.165.0"}})
	rows := promptRows("0.165.0", tonight, "run_hooks_and_record_inputs")

	_, _, hs := fieldDrift(cat, rows, ran, tonight)
	h := hs[0]
	want := MovedEvent{Event: promptEvent, From: "codex.turn_input", To: "run_hooks_and_record_inputs", Keys: 2}
	if len(h.Moved) != 1 || h.Moved[0] != want || len(h.Unseen)+len(h.NewSurfaces) != 0 {
		t.Fatalf("moved %+v, unseen %+v, new %+v", h.Moved, h.Unseen, h.NewSurfaces)
	}
	if got := h.headline(); !strings.HasSuffix(got, "): 1 span event moved") {
		t.Errorf("headline %q", got)
	}
	d := Drift{Harnesses: hs}
	line := "`" + promptEvent + "` (2 keys): from span `codex.turn_input` to `run_hooks_and_record_inputs`"
	if md := d.markdown(); !strings.Contains(md, "- "+line) || strings.Contains(md, removedMark) || strings.Contains(md, addedMark) {
		t.Errorf("digest:\n%s", md)
	}
	// Unsigned, it joins the headline on Slack's own bar: no green or red attachment.
	if items := slackItems(t, d); !slices.ContainsFunc(items, func(i slackItem) bool {
		return i.colour == "" && strings.Contains(i.text, "_Span events on another span, keys unchanged_\n• "+line)
	}) || slices.ContainsFunc(items, func(i slackItem) bool { return i.colour != "" }) {
		t.Errorf("slack: %+v", items)
	}
	if f := h.fieldsOf("0.165.0", rows, ran); f == nil || f.Moved != 1 || f.NewSurfaces+f.Unseen != 0 {
		t.Errorf("release fields %+v", f)
	}
}

// A span event is no move where it is not one event changing span in the last build judged:
// each is said as surfaces gone and new, as before.
func TestASpanEventThatIsNoMove(t *testing.T) {
	day := time.Date(2026, 10, 27, 4, 0, 0, 0, time.UTC)
	tonight := day.Add(24 * time.Hour)
	var cat Catalog
	mergeFields(&cat, promptRows("0.164.0", day, "codex.turn_input", "session_init"), nil)
	at := func(version string, spans ...string) []e2e.FieldRow { return promptRows(version, tonight, spans...) }
	const old, moved, initSpan = "codex.turn_input", "run_hooks_and_record_inputs", "session_init"
	whole := func(versions ...string) censusRuns {
		var rs []e2e.CensusRun
		for _, v := range versions {
			rs = append(rs, e2e.CensusRun{Harness: "codex", Version: v})
		}
		return censusRan(rs)
	}
	partialFirst := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.165.0", Failed: true}, {Harness: "codex", Version: "0.166.0"}})
	partialLast := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.165.0"}, {Harness: "codex", Version: "0.166.0"},
		{Harness: "codex", Version: "0.167.0", Failed: true}})
	for _, c := range []struct {
		name        string
		rows        []e2e.FieldRow
		ran         censusRuns
		unseen, new int
	}{
		{"keys changed", at("0.165.0", moved+":slug", initSpan), whole("0.165.0"), 1, 1},
		// session_init is gone too, and carries the same event name.
		{"two surfaces gone", at("0.165.0", moved), whole("0.165.0"), 2, 1},
		{"two surfaces new", at("0.165.0", moved, "other_span", initSpan), whole("0.165.0"), 1, 2},
		{"moved in an earlier build", slices.Concat(at("0.165.0", moved, initSpan), at("0.166.0", moved, initSpan)),
			whole("0.165.0", "0.166.0"), 1, 1},
		{"on both spans in an earlier build", slices.Concat(at("0.165.0", old, moved, initSpan), at("0.166.0", moved, initSpan)),
			whole("0.165.0", "0.166.0"), 1, 1},
		{"on both spans in an earlier, partial build", slices.Concat(at("0.165.0", old, moved, initSpan), at("0.166.0", moved, initSpan)),
			partialFirst, 1, 1},
		// The old span goes in 0.166.0, the last judged; only the partial 0.167.0 sends the new.
		{"sent only by a later, partial build", slices.Concat(at("0.165.0", old, initSpan), at("0.166.0", initSpan), at("0.167.0", moved)),
			partialLast, 1, 1},
		{"gone in an earlier build and back", slices.Concat(at("0.165.0", initSpan), at("0.166.0", old, initSpan), at("0.167.0", old, moved, initSpan)),
			whole("0.165.0", "0.166.0", "0.167.0"), 1, 1},
		{"partial", at("0.165.0", moved, initSpan), censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.165.0", Failed: true}}), 0, 1},
	} {
		_, _, hs := fieldDrift(cat, c.rows, c.ran, tonight)
		if h := hs[0]; len(h.Moved) != 0 || len(h.Unseen) != c.unseen || len(h.NewSurfaces) != c.new {
			t.Errorf("%s: moved %+v, unseen %+v, new %+v", c.name, h.Moved, h.Unseen, h.NewSurfaces)
		}
	}
}

// promptEvent is the name tracing gives Codex's user_prompt event, after its file.
const promptEvent = "event otel/src/events/session_telemetry.rs"

// promptRows are what a build sent: a log it always sends, and promptEvent on each span, with
// its keys after a colon; without them session_init's are slug, any other span's prompt_length
// and slug.
func promptRows(version string, at time.Time, spans ...string) []e2e.FieldRow {
	out := []e2e.FieldRow{field(version, "logs/codex.api_request", "model", "safe", at)}
	for _, span := range spans {
		span, keys, ok := strings.Cut(span, ":")
		switch {
		case !ok && span == "session_init":
			keys = "slug"
		case !ok:
			keys = "prompt_length,slug"
		}
		for k := range strings.SplitSeq(keys, ",") {
			out = append(out, field(version, "traces/"+span+"/events/"+promptEvent, k, "safe", at))
		}
	}
	return out
}
