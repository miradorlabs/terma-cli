package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

var t0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T, dir, sid string, at time.Time) string {
	t.Helper()
	id, err := Start(dir, sid, at)
	must(t, err)
	return id
}

func mixedAt(t *testing.T, dir, id string, at time.Time) (time.Time, bool) {
	t.Helper()
	return MixedAt(dir, id, at)
}

// The ledger keeps every run of a session in the file the plan names, and a crossing
// marks the current run alone.
func TestRunsAndTheirCrossings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r1 := start(t, dir, "sess", t0)
	r2 := start(t, dir, "sess", t0.Add(time.Hour))
	if r1 == "" || r1 == r2 {
		t.Fatalf("run ids %q, %q", r1, r2)
	}
	if _, mixed := mixedAt(t, dir, "sess", t0.Add(2*time.Hour)); mixed {
		t.Fatal("a run nothing marked is Mixed")
	}
	crossed := t0.Add(90 * time.Minute)
	must(t, MarkMixed(dir, "sess", crossed, crossing))

	for _, c := range []struct {
		name  string
		at    time.Time
		mixed bool
	}{
		{"the first run", t0.Add(time.Minute), false},
		{"the current run", t0.Add(2 * time.Hour), true},
		{"the current run before the crossing", t0.Add(61 * time.Minute), true},
		{"a record with no time", time.Time{}, true},
		{"a record older than every run", t0.Add(-time.Minute), false},
	} {
		got, mixed := mixedAt(t, dir, "sess", c.at)
		if mixed != c.mixed || mixed && !got.Equal(crossed) {
			t.Errorf("%s: MixedAt = %v, %v; want mixed %v from %v", c.name, got, mixed, c.mixed, crossed)
		}
	}

	b, err := os.ReadFile(filepath.Join(dir, Dir, "sess.json"))
	must(t, err)
	var raw struct {
		Runs []map[string]any `json:"runs"`
	}
	must(t, json.Unmarshal(b, &raw))
	if len(raw.Runs) != 2 || raw.Runs[0]["id"] != r1 || raw.Runs[0]["started"] != "2026-11-12T12:00:00Z" ||
		raw.Runs[0]["mixed_at"] != nil || raw.Runs[1]["mixed_at"] != "2026-11-12T13:30:00Z" {
		t.Fatalf("ledger on disk: %s", b)
	}
}

// Mixed only turns on: a later crossing keeps the earlier time, an earlier one moves it
// earlier, and only a new run is clear of it.
func TestMixedOnlyTurnsOn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start(t, dir, "sess", t0)
	must(t, MarkMixed(dir, "sess", t0.Add(10*time.Minute), crossing))
	must(t, MarkMixed(dir, "sess", t0.Add(20*time.Minute), crossing))
	if got, _ := mixedAt(t, dir, "sess", t0.Add(time.Hour)); !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("a later crossing moved the time to %v", got)
	}
	must(t, MarkMixed(dir, "sess", t0.Add(5*time.Minute), crossing))
	if got, _ := mixedAt(t, dir, "sess", t0.Add(time.Hour)); !got.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("an earlier crossing left the time at %v", got)
	}
	start(t, dir, "sess", t0.Add(2*time.Hour))
	if _, mixed := mixedAt(t, dir, "sess", t0.Add(3*time.Hour)); mixed {
		t.Fatal("a resume is still Mixed")
	}
	if _, mixed := mixedAt(t, dir, "sess", t0.Add(time.Hour)); !mixed {
		t.Fatal("the resume cleared the earlier run's crossing")
	}
}

// A crossing seen before any run was recorded makes a run covering every record.
func TestACrossingBeforeAnyStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, mixed := mixedAt(t, dir, "sess", t0); mixed {
		t.Fatal("a session with no ledger is Mixed")
	}
	must(t, MarkMixed(dir, "sess", t0, crossing))
	for _, at := range []time.Time{t0.Add(-24 * time.Hour), t0, {}} {
		if got, mixed := mixedAt(t, dir, "sess", at); !mixed || !got.Equal(t0) {
			t.Errorf("MixedAt(%v) = %v, %v", at, got, mixed)
		}
	}
}

// A subagent's ID shares its session's runs: MixedAt answers for it, from the session's
// ledger.
func TestAliases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start(t, dir, "sess", t0)
	must(t, Alias(dir, "sess", "agent-1"))
	must(t, Alias(dir, "sess", "agent-1"))
	must(t, Alias(dir, "sess", "sess"))
	if _, mixed := mixedAt(t, dir, "agent-1", t0.Add(time.Minute)); mixed {
		t.Fatal("an alias of a clean run is Mixed")
	}
	must(t, MarkMixed(dir, "sess", t0.Add(time.Minute), crossing))
	if got, mixed := mixedAt(t, dir, "agent-1", t0.Add(time.Hour)); !mixed || !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("the alias's MixedAt = %v, %v", got, mixed)
	}
	b, err := os.ReadFile(filepath.Join(dir, Dir, "sess.json"))
	must(t, err)
	var l ledger
	must(t, json.Unmarshal(b, &l))
	if len(l.Aliases) != 1 || l.Aliases[0] != "agent-1" {
		t.Fatalf("aliases %v", l.Aliases)
	}
	if _, mixed := mixedAt(t, dir, "agent-2", t0.Add(time.Hour)); mixed {
		t.Fatal("an unknown ID is Mixed")
	}
}

// Session IDs and aliases are held to what a file name may be, as claims are; an ID that
// cannot be one has no ledger.
func TestInvalidIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, bad := range []string{"", "../x", ".hidden", "a/b"} {
		if _, err := Start(dir, bad, t0); err == nil {
			t.Errorf("Start(%q) succeeded", bad)
		}
		if err := MarkMixed(dir, bad, t0, crossing); err == nil {
			t.Errorf("MarkMixed(%q, crossing) succeeded", bad)
		}
		if err := Alias(dir, "sess", bad); err == nil {
			t.Errorf("Alias(sess, %q) succeeded", bad)
		}
		if _, mixed := MixedAt(dir, bad, t0); mixed {
			t.Errorf("MixedAt(%q) is Mixed", bad)
		}
	}
}

// A ledger that cannot be read may hold a crossing: it reads as Mixed from the start.
func TestAnUnreadableLedgerIsMixed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start(t, dir, "sess", t0)
	must(t, os.WriteFile(filepath.Join(dir, Dir, "sess.json"), []byte("{"), 0o600))
	if got, mixed := mixedAt(t, dir, "sess", t0); !mixed || !got.IsZero() {
		t.Fatalf("MixedAt = %v, %v", got, mixed)
	}
	if _, err := Start(dir, "sess", t0); err == nil {
		t.Fatal("Start wrote over an unreadable ledger")
	}
}

// Concurrent hooks lose nothing: every run and the crossing are kept.
func TestConcurrentWriters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if _, err := Start(dir, "sess", t0.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	must(t, MarkMixed(dir, "sess", t0.Add(time.Hour), crossing))
	b, err := os.ReadFile(filepath.Join(dir, Dir, "sess.json"))
	must(t, err)
	var l ledger
	must(t, json.Unmarshal(b, &l))
	if len(l.Runs) != 16 {
		t.Fatalf("%d runs after 16 starts", len(l.Runs))
	}
}

// The ledger outlives everything it could drop.
func TestRetentionOutlivesTheQueues(t *testing.T) {
	t.Parallel()
	if Retention <= spool.MaxAge {
		t.Fatalf("Retention %v does not outlive the spool's %v", Retention, spool.MaxAge)
	}
}

// Current is the latest run a session began, known by the session's ID or an alias's, as
// long as the ledger is kept; Aliases are every subagent ID that shares its runs.
func TestCurrentAndAliases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if run, ok := Current(dir, "sess"); ok || run != "" {
		t.Fatalf("a session with no run is in %q", run)
	}
	if got, err := Aliases(dir, "sess"); err != nil || len(got) != 0 {
		t.Fatalf("a session with no ledger has aliases %v, %v", got, err)
	}
	r1 := start(t, dir, "sess", t0)
	must(t, Alias(dir, "sess", "agent-1"))
	must(t, Alias(dir, "sess", "agent-2"))
	must(t, Alias(dir, "sess", "agent-1"))
	for _, id := range []string{"sess", "agent-1", "agent-2"} {
		if run, ok := Current(dir, id); !ok || run != r1 {
			t.Fatalf("Current(%s) = %q, %v; want %q", id, run, ok, r1)
		}
	}
	r2 := start(t, dir, "sess", t0.Add(time.Hour))
	if run, ok := Current(dir, "agent-2"); !ok || run != r2 {
		t.Fatalf("after a resume, Current = %q, %v; want %q", run, ok, r2)
	}
	if got, err := Aliases(dir, "sess"); err != nil || len(got) != 2 || got[0] != "agent-1" || got[1] != "agent-2" {
		t.Fatalf("Aliases = %v, %v", got, err)
	}
	if _, ok := Current(dir, "../escape"); ok {
		t.Fatal("an ID no file can be named after has a run")
	}
	if _, err := Aliases(dir, "../escape"); err == nil {
		t.Fatal("aliases of an ID no file can be named after")
	}
	must(t, os.WriteFile(filepath.Join(dir, Dir, "broken.json"), []byte("{"), 0o600))
	if _, ok := Current(dir, "broken"); ok {
		t.Fatal("an unreadable ledger has a current run")
	}
	if _, err := Aliases(dir, "broken"); err == nil {
		t.Fatal("an unreadable ledger lists no aliases without an error")
	}
}

// Mixed lists the sessions whose current run crossed organizations, and no other: not a
// session whose Mixed run a resume ended, nor an alias, nor a file no session names.
func TestMixedListsTheSessionsThatCrossed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start(t, dir, "calm", t0)
	start(t, dir, "crossed", t0)
	must(t, Alias(dir, "crossed", "crossed-agent"))
	must(t, MarkMixed(dir, "crossed", t0.Add(time.Minute), crossing))
	start(t, dir, "resumed", t0)
	must(t, MarkMixed(dir, "resumed", t0.Add(time.Minute), crossing))
	start(t, dir, "resumed", t0.Add(time.Hour))
	must(t, os.WriteFile(filepath.Join(dir, Dir, ".tmp-x.json"), []byte("{"), 0o600))
	if got := Mixed(dir, t0.Add(2*time.Hour), 24*time.Hour); !slices.Equal(got, []MixedRun{{Session: "crossed", Why: crossing, Recent: true}}) {
		t.Fatalf("Mixed = %+v", got)
	}
	if got := Mixed(t.TempDir(), t0, 24*time.Hour); got != nil {
		t.Fatalf("Mixed with no ledger = %+v", got)
	}
	// Doctor names only the runs active lately, and counts the others.
	if got := Mixed(dir, t0.Add(48*time.Hour), 24*time.Hour); !slices.Equal(got, []MixedRun{{Session: "crossed", Why: crossing}}) {
		t.Fatalf("a day later, Mixed = %+v", got)
	}
}

// crossing is the reason a test marks a run Mixed for.
var crossing = Why{Reason: Crossed}

// touch is Touch, failing the test on an error, returning what the run records after it.
func touch(t *testing.T, dir, sid, run string, locs []string) []string {
	t.Helper()
	merged, err := Touch(dir, sid, run, locs)
	must(t, err)
	return merged
}

// Touch returns every location the run records once it has written, read under its lock:
// a hook that read the ledger before another hook of the run touched it learns of the
// other hook's locations here, whether or not its own were new.
func TestTouchReturnsEveryLocationTheRunRecords(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := start(t, dir, "sess", t0)
	_, stale, _ := Locations(dir, "sess")
	touch(t, dir, "sess", run, []string{"github.com/acme/api"})
	if got := touch(t, dir, "sess", run, []string{"github.com/dawson/side"}); !slices.Equal(got, []string{"github.com/acme/api", "github.com/dawson/side"}) || len(stale) != 0 {
		t.Fatalf("Touch returned %v after a read of %v", got, stale)
	}
	if got := touch(t, dir, "sess", run, []string{"github.com/acme/api"}); !slices.Equal(got, []string{"github.com/acme/api", "github.com/dawson/side"}) {
		t.Fatalf("Touch with nothing new returned %v", got)
	}
}

// locations is what Locations reads of id's current run.
func locations(t *testing.T, dir, id string) (string, []string) {
	t.Helper()
	run, locs, ok := Locations(dir, id)
	if !ok {
		t.Fatalf("no current run for %s", id)
	}
	return run, locs
}

// A run keeps each location it was placed in or seen touching once, in the order first
// recorded, read through an alias too; a resume begins a run that has been nowhere, and a
// location for an earlier run is that run's.
func TestARunRemembersItsLocations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r1 := start(t, dir, "sess", t0)
	must(t, Alias(dir, "sess", "agent-1"))
	touch(t, dir, "sess", r1, []string{"github.com/acme/api", "outside-git"})
	touch(t, dir, "sess", "", []string{"github.com/acme/api", "github.com/dawson/side", ""})
	for _, id := range []string{"sess", "agent-1"} {
		if run, locs := locations(t, dir, id); run != r1 || !slices.Equal(locs, []string{"github.com/acme/api", "outside-git", "github.com/dawson/side"}) {
			t.Fatalf("Locations(%s) = %q, %v", id, run, locs)
		}
	}

	r2 := start(t, dir, "sess", t0.Add(time.Hour))
	if run, locs := locations(t, dir, "sess"); run != r2 || len(locs) != 0 {
		t.Fatalf("the resumed run %q has been to %v", run, locs)
	}
	touch(t, dir, "sess", r1, []string{"github.com/acme/web"})
	touch(t, dir, "sess", "unknown-run", []string{"github.com/dawson/terma"})
	if _, locs := locations(t, dir, "sess"); !slices.Equal(locs, []string{"github.com/dawson/terma"}) {
		t.Fatalf("the current run has been to %v", locs)
	}
	b, err := os.ReadFile(filepath.Join(dir, Dir, "sess.json"))
	must(t, err)
	var raw struct {
		Runs []struct {
			Locations []string `json:"locations"`
		} `json:"runs"`
	}
	must(t, json.Unmarshal(b, &raw))
	if len(raw.Runs) != 2 || !slices.Equal(raw.Runs[0].Locations, []string{"github.com/acme/api", "outside-git", "github.com/dawson/side", "github.com/acme/web"}) {
		t.Fatalf("ledger on disk: %s", b)
	}

	if _, _, ok := Locations(dir, "../escape"); ok {
		t.Fatal("an ID no file can be named after has a run")
	}
	must(t, os.WriteFile(filepath.Join(dir, Dir, "broken.json"), []byte("{"), 0o600))
	if _, _, ok := Locations(dir, "broken"); ok {
		t.Fatal("an unreadable ledger has a current run")
	}
	if _, err := Touch(dir, "broken", "", []string{"github.com/acme/api"}); err == nil {
		t.Fatal("touched an unreadable ledger")
	}
}

// Touch writes the ledger only when a location is new to the run.
func TestTouchWritesOnlyANewLocation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, Dir, "sess.json")
	stat := func() os.FileInfo {
		t.Helper()
		fi, err := os.Stat(path)
		must(t, err)
		return fi
	}
	run := start(t, dir, "sess", t0)
	touch(t, dir, "sess", run, []string{"github.com/acme/api"})
	before := stat()
	touch(t, dir, "sess", run, []string{"github.com/acme/api"})
	touch(t, dir, "sess", "", nil)
	if !os.SameFile(before, stat()) {
		t.Fatal("rewrote the ledger with nothing new")
	}
	touch(t, dir, "sess", run, []string{"github.com/acme/api", "github.com/acme/web"})
	if os.SameFile(before, stat()) {
		t.Fatal("a new location left the ledger as it was")
	}
}

// A location seen before any run was recorded starts one, as a crossing does.
func TestATouchBeforeAnyRunStartsOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	touch(t, dir, "sess", "", []string{"github.com/acme/api"})
	run, locs := locations(t, dir, "sess")
	if run == "" || !slices.Equal(locs, []string{"github.com/acme/api"}) {
		t.Fatalf("Locations = %q, %v", run, locs)
	}
	if _, mixed := mixedAt(t, dir, "sess", t0); mixed {
		t.Fatal("a run only touched is Mixed")
	}
}

// A Mixed mark records why, and an earlier mark wins with its reason. A ledger written
// before reasons were recorded reads as Crossed; one that cannot be read, or a run that is
// not Mixed, has none.
func TestAMixedMarkRecordsWhy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start(t, dir, "s", t0)
	unknown := Why{Reason: UnknownReach, Where: "cat $UNSET/x"}
	if got := Read(dir, "s").Why(time.Time{}); got != (Why{}) {
		t.Fatalf("a run not Mixed reads %+v", got)
	}
	must(t, MarkMixed(dir, "s", t0.Add(2*time.Minute), unknown))
	must(t, MarkMixed(dir, "s", t0.Add(3*time.Minute), Why{Reason: LocationBound}))
	if got := Read(dir, "s").Why(time.Time{}); got != unknown {
		t.Fatalf("Why = %+v, want %+v", got, unknown)
	}
	must(t, MarkMixed(dir, "s", t0.Add(time.Minute), Why{Reason: Crossed, Where: "github.com/acme/api"}))
	if got := Read(dir, "s").Why(time.Time{}); got != (Why{Reason: Crossed, Where: "github.com/acme/api"}) {
		t.Fatalf("an earlier mark: Why = %+v", got)
	}

	old := `{"runs":[{"id":"r1","started":"2026-11-12T12:00:00Z","mixed_at":"2026-11-12T12:01:00Z"}]}`
	must(t, os.WriteFile(filepath.Join(dir, Dir, "old.json"), []byte(old), 0o600))
	if got := Read(dir, "old").Why(time.Time{}); got != (Why{Reason: Crossed}) {
		t.Fatalf("a ledger from before reasons: Why = %+v", got)
	}
	if got := Mixed(dir, t0.Add(time.Hour), 24*time.Hour); len(got) != 2 || got[0].Why != (Why{Reason: Crossed}) || got[1].Why.Where != "github.com/acme/api" {
		t.Fatalf("Mixed = %+v", got)
	}
	must(t, os.WriteFile(filepath.Join(dir, Dir, "bad.json"), []byte("{"), 0o600))
	if got := Read(dir, "bad").Why(time.Time{}); got != (Why{}) {
		t.Fatalf("an unreadable ledger: Why = %+v", got)
	}
}

// A run holds at most MaxLocations distinct locations: up to MaxLocations (and repeats of
// them) stays non-Mixed, and the next distinct location caps the slice and marks the run
// Mixed with LocationBound.
func TestTouchEnforcesMaxLocations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := start(t, dir, "bounded", t0)

	locs := make([]string, MaxLocations)
	for i := range locs {
		locs[i] = fmt.Sprintf("github.com/acme/r%02d", i)
	}
	got := touch(t, dir, "bounded", r, locs)
	if len(got) != MaxLocations {
		t.Fatalf("len(Locations) = %d, want %d", len(got), MaxLocations)
	}
	if _, mixed := mixedAt(t, dir, "bounded", time.Now()); mixed {
		t.Fatal("run marked Mixed at exactly MaxLocations")
	}
	// Repeating already-known locations must not trigger LocationBound.
	got = touch(t, dir, "bounded", r, []string{locs[0], locs[MaxLocations-1]})
	if len(got) != MaxLocations {
		t.Fatalf("len(Locations) after repeats = %d, want %d", len(got), MaxLocations)
	}
	if _, mixed := mixedAt(t, dir, "bounded", time.Now()); mixed {
		t.Fatal("run marked Mixed after repeating known locations")
	}
	// The (MaxLocations+1)th distinct location caps the slice and marks Mixed(LocationBound).
	got = touch(t, dir, "bounded", r, []string{"github.com/acme/overflow"})
	if len(got) != MaxLocations {
		t.Fatalf("len(Locations) after overflow = %d, want %d", len(got), MaxLocations)
	}
	snap := Read(dir, "bounded")
	if _, mixed := snap.MixedAt(time.Time{}); !mixed {
		t.Fatal("run was not marked Mixed after exceeding MaxLocations")
	}
	if why := snap.Why(time.Time{}); why != (Why{Reason: LocationBound}) {
		t.Fatalf("Why = %+v, want LocationBound", why)
	}

	// One batch that crosses MaxLocations keeps MaxLocations and marks Mixed(LocationBound).
	rBatch := start(t, dir, "batch", t0)
	touch(t, dir, "batch", rBatch, locs[:MaxLocations-1])
	got = touch(t, dir, "batch", rBatch, []string{"github.com/acme/b1", "github.com/acme/b2", "github.com/acme/b3"})
	if len(got) != MaxLocations {
		t.Fatalf("batch overflow len = %d, want %d", len(got), MaxLocations)
	}
	if why := Read(dir, "batch").Why(time.Time{}); why != (Why{Reason: LocationBound}) {
		t.Fatalf("batch overflow Why = %+v, want LocationBound", why)
	}

	// A run already Mixed with Crossed preserves that reason when it later hits MaxLocations.
	rCrossed := start(t, dir, "crossed-bound", t0)
	must(t, MarkMixed(dir, "crossed-bound", t0.Add(time.Minute), Why{Reason: Crossed, Where: "github.com/other/repo"}))
	touch(t, dir, "crossed-bound", rCrossed, locs)
	touch(t, dir, "crossed-bound", rCrossed, []string{"github.com/acme/overflow"})
	if why := Read(dir, "crossed-bound").Why(time.Time{}); why != (Why{Reason: Crossed, Where: "github.com/other/repo"}) {
		t.Fatalf("already-Crossed run Why = %+v, want Crossed preserved", why)
	}
}

// Concurrent or out-of-order Start calls keep Runs ordered by Started so MarkMixed and
// Locate target the same latest run that timestamped MixedAt selects.
func TestOutOfOrderStartsStayConsistentWithMarkMixed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	later := start(t, dir, "ooo", t0.Add(10*time.Minute))
	_ = start(t, dir, "ooo", t0)
	if cur, ok := Current(dir, "ooo"); !ok || cur != later {
		t.Fatalf("Current = %q, %v; want later run %q", cur, ok, later)
	}
	cross := t0.Add(12 * time.Minute)
	must(t, MarkMixed(dir, "ooo", cross, Why{Reason: Crossed, Where: "github.com/other/repo"}))
	snap := Read(dir, "ooo")
	if _, mixed := snap.MixedAt(t0.Add(time.Minute)); mixed {
		t.Fatal("earlier clean run was marked Mixed instead of the latest-started run")
	}
	if at, mixed := snap.MixedAt(t0.Add(15 * time.Minute)); !mixed || !at.Equal(cross) {
		t.Fatalf("latest run MixedAt = %v, %v; want %v, true", at, mixed, cross)
	}
}

// An alias mapping takes precedence over a pre-existing direct ledger for the same ID,
// and any Mixed mark on either side is preserved so neither order hides Mixed.
func TestAliasPrecedenceAndMixedMerge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Start(child) -> Alias(parent, child) -> MarkMixed(parent) must leave child reading Mixed.
	start(t, dir, "child-a", t0)
	start(t, dir, "parent-a", t0)
	must(t, Alias(dir, "parent-a", "child-a"))
	cross := t0.Add(5 * time.Minute)
	must(t, MarkMixed(dir, "parent-a", cross, Why{Reason: Crossed, Where: "github.com/other/repo"}))
	if at, mixed := mixedAt(t, dir, "child-a", t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
		t.Fatalf("child-a after parent MarkMixed: MixedAt = %v, %v; want %v, true", at, mixed, cross)
	}
	if pos, ok := Locate(dir, "child-a"); !ok || !pos.MixedAt.Equal(cross) {
		t.Fatalf("Locate(child-a) = %+v, %v; want parent MixedAt %v", pos, ok, cross)
	}
	// Mixed must emit only canonical parent-a, not duplicate alias child-a even though child-a.json exists.
	if got := Mixed(dir, t0.Add(6*time.Minute), time.Hour); len(got) != 1 || got[0].Session != "parent-a" {
		t.Fatalf("Mixed = %+v, want only [parent-a]", got)
	}

	// Start(child) -> MarkMixed(child) -> Start(parent) -> Alias(parent, child) reconciles into parent, sibling, and Mixed.
	start(t, dir, "child-b", t0)
	crossB := t0.Add(2 * time.Minute)
	must(t, MarkMixed(dir, "child-b", crossB, Why{Reason: UnknownReach, Where: "eval"}))
	start(t, dir, "parent-b", t0)
	must(t, Alias(dir, "parent-b", "sibling-b"))
	must(t, Alias(dir, "parent-b", "child-b"))
	for _, id := range []string{"child-b", "parent-b", "sibling-b"} {
		if at, mixed := mixedAt(t, dir, id, t0.Add(3*time.Minute)); !mixed || !at.Equal(crossB) {
			t.Fatalf("%s after pre-alias MarkMixed: MixedAt = %v, %v; want %v, true", id, at, mixed, crossB)
		}
	}
	// Alias(parent, child) -> MarkMixed(child) writes to canonical parent so parent and sibling read Mixed.
	start(t, dir, "parent-d", t0)
	must(t, Alias(dir, "parent-d", "child-d"))
	must(t, Alias(dir, "parent-d", "sibling-d"))
	must(t, MarkMixed(dir, "child-d", crossB, Why{Reason: Crossed, Where: "other"}))
	for _, id := range []string{"parent-d", "child-d", "sibling-d"} {
		if at, mixed := mixedAt(t, dir, id, t0.Add(3*time.Minute)); !mixed || !at.Equal(crossB) {
			t.Fatalf("%s after post-alias MarkMixed(child): MixedAt = %v, %v; want %v, true", id, at, mixed, crossB)
		}
	}

	// Unstarted parent with alias still preserves child's Mixed mark and appears in Mixed().
	start(t, dir, "child-c", t0)
	must(t, MarkMixed(dir, "child-c", crossB, Why{Reason: Crossed}))
	must(t, Alias(dir, "parent-c", "child-c"))
	for _, id := range []string{"child-c", "parent-c"} {
		if _, mixed := mixedAt(t, dir, id, t0.Add(3*time.Minute)); !mixed {
			t.Fatalf("%s aliased to unstarted parent lost its Mixed mark", id)
		}
	}
	gotMixed := Mixed(dir, t0.Add(6*time.Minute), time.Hour)
	var names []string
	for _, m := range gotMixed {
		names = append(names, m.Session)
	}
	if !slices.Equal(names, []string{"parent-a", "parent-b", "parent-c", "parent-d"}) {
		t.Fatalf("Mixed sessions = %v, want [parent-a parent-b parent-c parent-d]", names)
	}

	// Transitive alias chain: Alias(A, B) then Alias(C, A) then MarkMixed(C) must propagate to B.
	start(t, dir, "chain-a", t0)
	start(t, dir, "chain-b", t0)
	start(t, dir, "chain-c", t0)
	must(t, Alias(dir, "chain-a", "chain-b"))
	must(t, Alias(dir, "chain-c", "chain-a"))
	must(t, MarkMixed(dir, "chain-c", crossB, Why{Reason: Crossed, Where: "transitive"}))
	for _, id := range []string{"chain-a", "chain-b", "chain-c"} {
		if at, mixed := mixedAt(t, dir, id, t0.Add(3*time.Minute)); !mixed || !at.Equal(crossB) {
			t.Fatalf("transitive %s MixedAt = %v, %v; want %v, true", id, at, mixed, crossB)
		}
	}
	if pos, ok := Locate(dir, "chain-c"); !ok || !slices.Contains(pos.Aliases, "chain-a") || !slices.Contains(pos.Aliases, "chain-b") {
		t.Fatalf("Locate(chain-c).Aliases = %v, ok=%v; want chain-a and chain-b", pos.Aliases, ok)
	}
	if got, err := Aliases(dir, "chain-c"); err != nil || !slices.Contains(got, "chain-a") || !slices.Contains(got, "chain-b") {
		t.Fatalf("Aliases(chain-c) = %v, %v; want chain-a and chain-b", got, err)
	}
	// Alias(chain-b, chain-c) must be a no-op rather than creating a cycle.
	must(t, Alias(dir, "chain-b", "chain-c"))
	if _, mixed := mixedAt(t, dir, "chain-b", t0.Add(3*time.Minute)); !mixed {
		t.Fatal("reverse alias broke chain-b Mixed state")
	}

	// Corrupted alias cycle on disk fails closed.
	must(t, os.WriteFile(aliasPath(dir, "cyc-a"), []byte("cyc-b"), 0o600))
	must(t, os.WriteFile(aliasPath(dir, "cyc-b"), []byte("cyc-a"), 0o600))
	if _, mixed := Read(dir, "cyc-a").MixedAt(t0); !mixed {
		t.Fatal("alias cycle did not fail closed")
	}

	// Empty alias file fails closed.
	must(t, os.WriteFile(aliasPath(dir, "bad-alias"), nil, 0o600))
	if _, mixed := Read(dir, "bad-alias").MixedAt(t0); !mixed {
		t.Fatal("empty alias file did not fail closed")
	}
}

// Deterministic + stress concurrency test: a writer that resolved child before Alias publishes
// must revalidate under lock (or mirror sidecar) so parent, sibling, child, and Mixed() never miss Mixed.
func TestAliasConcurrentWriteLinearizable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cross := t0.Add(5 * time.Minute)

	// Deterministic lock-hold: hold parent.lock while starting MarkMixed(child) and Alias(parent, child),
	// and also hold child.lock so both queue on the lock and revalidate after Alias publishes.
	for _, holdTarget := range []string{"det-parent", "det-child"} {
		sub := filepath.Join(dir, holdTarget)
		start(t, sub, "det-parent", t0)
		start(t, sub, "det-child", t0)
		must(t, Alias(sub, "det-parent", "det-sibling"))
		lockPath := filepath.Join(sub, Dir, holdTarget+".json.lock")
		unlock, err := flock.TryLock(lockPath)
		if err != nil {
			t.Fatalf("TryLock(%s): %v", holdTarget, err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := MarkMixed(sub, "det-child", cross, Why{Reason: Crossed, Where: "race"}); err != nil {
				t.Errorf("MarkMixed: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := Alias(sub, "det-parent", "det-child"); err != nil {
				t.Errorf("Alias: %v", err)
			}
		}()
		time.Sleep(25 * time.Millisecond)
		unlock()
		wg.Wait()
		for _, id := range []string{"det-parent", "det-child", "det-sibling"} {
			if at, mixed := mixedAt(t, sub, id, t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
				t.Fatalf("hold %s: %s MixedAt = %v, %v; want %v, true", holdTarget, id, at, mixed, cross)
			}
		}
		if got := Mixed(sub, t0.Add(6*time.Minute), time.Hour); len(got) != 1 || got[0].Session != "det-parent" {
			t.Fatalf("hold %s: Mixed = %+v, want [det-parent]", holdTarget, got)
		}
	}

	// Stress: concurrent MarkMixed/MarkAside vs Alias across 16 sessions each.
	for i := range 16 {
		p, c, s := fmt.Sprintf("sp-%d", i), fmt.Sprintf("sc-%d", i), fmt.Sprintf("ss-%d", i)
		start(t, dir, p, t0)
		start(t, dir, c, t0)
		must(t, Alias(dir, p, s))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				if err := MarkMixed(dir, c, cross, Why{Reason: Crossed}); err != nil {
					t.Errorf("MarkMixed(%s): %v", c, err)
				}
			} else {
				if err := MarkAside(dir, c, "", cross); err != nil {
					t.Errorf("MarkAside(%s): %v", c, err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			if err := Alias(dir, p, c); err != nil {
				t.Errorf("Alias(%s, %s): %v", p, c, err)
			}
		}()
		wg.Wait()
		for _, id := range []string{p, c, s} {
			if _, mixed := mixedAt(t, dir, id, t0.Add(6*time.Minute)); !mixed {
				t.Fatalf("stress %d: %s missed Mixed", i, id)
			}
		}
	}
}

// Deterministic barrier tests for chained-alias MarkAside mirroring and merge-before-pointer.
// Not parallel because it installs the package-level afterAsideWrite hook.
func TestMarkAsideChainedAliasAndMergeBeforePointer(t *testing.T) {
	dir := t.TempDir()
	cross := t0.Add(5 * time.Minute)

	// 1. Two chained concurrent parent changes during MarkAside(child):
	//    child.mixed lands -> Alias(parent, child) -> parent.mixed lands -> Alias(root, parent) -> root.mixed lands.
	start(t, dir, "ch-child", t0)
	start(t, dir, "ch-parent", t0)
	start(t, dir, "ch-root", t0)
	must(t, Alias(dir, "ch-root", "ch-sibling"))
	prevHook := afterAsideWrite
	t.Cleanup(func() { afterAsideWrite = prevHook })
	afterAsideWrite = func(id string) {
		switch id {
		case "ch-child":
			if err := Alias(dir, "ch-parent", "ch-child"); err != nil {
				t.Errorf("Alias(ch-parent, ch-child): %v", err)
			}
		case "ch-parent":
			if err := Alias(dir, "ch-root", "ch-parent"); err != nil {
				t.Errorf("Alias(ch-root, ch-parent): %v", err)
			}
		}
	}
	must(t, MarkAside(dir, "ch-child", "", cross))
	for _, id := range []string{"ch-child", "ch-parent", "ch-root", "ch-sibling"} {
		if at, mixed := mixedAt(t, dir, id, t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
			t.Fatalf("chained MarkAside: %s MixedAt = %v, %v; want %v, true", id, at, mixed, cross)
		}
	}

	// 2. Exhausting the hop bound during concurrent re-aliasing must return an error (fail closed).
	hops := 0
	afterAsideWrite = func(id string) {
		hops++
		next := fmt.Sprintf("loop-%d", hops)
		if err := Alias(dir, next, id); err != nil {
			t.Errorf("Alias(%s, %s): %v", next, id, err)
		}
	}
	if err := MarkAside(dir, "loop-0", "", cross); err == nil {
		t.Fatal("MarkAside returned nil after exhausting chained-alias retry bound")
	}
	afterAsideWrite = nil

	// 3. Merge-before-pointer: if the alias ledger is corrupt/unreadable, Alias fails without installing the pointer.
	start(t, dir, "clean-parent", t0)
	corruptChild := filepath.Join(dir, Dir, "corrupt-child.json")
	must(t, os.WriteFile(corruptChild, []byte("{bad json"), 0o600))
	if err := Alias(dir, "clean-parent", "corrupt-child"); err == nil {
		t.Fatal("Alias succeeded with unreadable child ledger")
	}
	if _, err := os.Lstat(aliasPath(dir, "corrupt-child")); !os.IsNotExist(err) {
		t.Fatalf("alias pointer was installed despite failed child read: %v", err)
	}
	if _, mixed := mixedAt(t, dir, "clean-parent", t0.Add(time.Minute)); mixed {
		t.Fatal("clean-parent was unexpectedly modified on failed Alias")
	}

	// 4. Re-aliasing a child from a Mixed parent to a new parent merges the old parent's Mixed state before replacing the pointer.
	start(t, dir, "re-old", t0)
	start(t, dir, "re-new", t0)
	must(t, Alias(dir, "re-old", "re-child"))
	must(t, Alias(dir, "re-new", "re-sibling"))
	must(t, MarkMixed(dir, "re-old", cross, Why{Reason: Crossed, Where: "old-parent"}))
	must(t, Alias(dir, "re-new", "re-child"))
	for _, id := range []string{"re-child", "re-new", "re-sibling"} {
		if at, mixed := mixedAt(t, dir, id, t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
			t.Fatalf("re-alias %s MixedAt = %v, %v; want %v, true", id, at, mixed, cross)
		}
	}

	// 5. Deterministic concurrent re-aliasing while holding old.json.lock:
	//    child -> old; hold old.json.lock; start MarkMixed(child) and Alias(new, child); release lock.
	//    Both must coordinate via ordered locking & sessionID revalidation so child, new, and sibling all read Mixed.
	for _, holdID := range []string{"cr-old", "cr-new", "cr-child"} {
		sub := filepath.Join(dir, "re-"+holdID)
		start(t, sub, "cr-old", t0)
		start(t, sub, "cr-new", t0)
		must(t, Alias(sub, "cr-old", "cr-child"))
		must(t, Alias(sub, "cr-new", "cr-sibling"))
		unlock, err := flock.TryLock(filepath.Join(sub, Dir, holdID+".json.lock"))
		must(t, err)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := MarkMixed(sub, "cr-child", cross, Why{Reason: Crossed, Where: "re-alias-race"}); err != nil {
				t.Errorf("MarkMixed(cr-child): %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := Alias(sub, "cr-new", "cr-child"); err != nil {
				t.Errorf("Alias(cr-new, cr-child): %v", err)
			}
		}()
		time.Sleep(25 * time.Millisecond)
		unlock()
		wg.Wait()
		for _, id := range []string{"cr-child", "cr-new", "cr-sibling"} {
			if at, mixed := mixedAt(t, sub, id, t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
				t.Fatalf("concurrent re-alias hold %s: %s MixedAt = %v, %v; want %v, true", holdID, id, at, mixed, cross)
			}
		}
	}

	// 6. Concurrent re-aliasing during MarkAside(child) when child already points to old:
	//    child -> old -> MarkAside(child) writes old.mixed -> Alias(new, child) -> MarkAside mirrors to new.mixed.
	start(t, dir, "ra-old", t0)
	start(t, dir, "ra-new", t0)
	must(t, Alias(dir, "ra-old", "ra-child"))
	must(t, Alias(dir, "ra-new", "ra-sibling"))
	afterAsideWrite = func(id string) {
		if id == "ra-old" {
			if err := Alias(dir, "ra-new", "ra-child"); err != nil {
				t.Errorf("Alias(ra-new, ra-child): %v", err)
			}
		}
	}
	must(t, MarkAside(dir, "ra-child", "", cross))
	afterAsideWrite = nil
	for _, id := range []string{"ra-child", "ra-new", "ra-sibling"} {
		if at, mixed := mixedAt(t, dir, id, t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
			t.Fatalf("MarkAside re-alias: %s MixedAt = %v, %v; want %v, true", id, at, mixed, cross)
		}
	}
}

func TestPruneAliasSidecarAndCanonicalLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cross := t0.Add(5 * time.Minute)
	agePastRetention := t0.Add(Retention + 24*time.Hour)
	agePastMixed := t0.Add(MixedRetention + 24*time.Hour)

	// 1. Sidecar-only canonical state: <canon>.json is absent, <canon>.mixed exists.
	//    After Retention, Prune must keep the alias so Read(alias) still sees Mixed.
	must(t, Alias(dir, "side-canon", "side-alias"))
	must(t, MarkAside(dir, "side-canon", "r1", cross))
	must(t, os.Remove(filepath.Join(dir, Dir, "side-canon.json")))
	old := t0
	must(t, os.Chtimes(aliasPath(dir, "side-alias"), old, old))
	Prune(context.Background(), dir, agePastRetention, nil)
	if at, mixed := mixedAt(t, dir, "side-alias", t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
		t.Fatalf("after Retention prune over sidecar-only canon: MixedAt = %v, %v; want %v, true", at, mixed, cross)
	}
	// Once MixedRetention elapses, both the sidecar and the stale alias are pruned.
	sideDir := filepath.Join(dir, Dir, "side-canon"+asideExt)
	entries, err := os.ReadDir(sideDir)
	must(t, err)
	for _, e := range entries {
		must(t, os.Chtimes(filepath.Join(sideDir, e.Name()), old, old))
	}
	must(t, os.Chtimes(sideDir, old, old))
	Prune(context.Background(), dir, agePastMixed, nil)
	if _, err := os.Lstat(aliasPath(dir, "side-alias")); !os.IsNotExist(err) {
		t.Fatalf("stale alias survived past MixedRetention after sidecar prune: %v", err)
	}

	// 2. Canonical lock held while pruning a stale alias pointing to an absent canonical ledger:
	//    Prune must not unlink the alias while a canonical writer holds <canon>.json.lock.
	must(t, Alias(dir, "lock-canon", "lock-alias"))
	must(t, os.Remove(filepath.Join(dir, Dir, "lock-canon.json")))
	must(t, os.Chtimes(aliasPath(dir, "lock-alias"), old, old))
	unlock, err := flock.TryLock(filepath.Join(dir, Dir, "lock-canon.json.lock"))
	must(t, err)
	Prune(context.Background(), dir, agePastRetention, nil)
	if _, err := os.Lstat(aliasPath(dir, "lock-alias")); err != nil {
		unlock()
		t.Fatalf("Prune unlinked alias while canonical lock was held: %v", err)
	}
	unlock()
	must(t, MarkMixed(dir, "lock-alias", cross, Why{Reason: Crossed}))
	if at, mixed := mixedAt(t, dir, "lock-alias", t0.Add(6*time.Minute)); !mixed || !at.Equal(cross) {
		t.Fatalf("after canonical write: lock-alias MixedAt = %v, %v; want %v, true", at, mixed, cross)
	}
}
