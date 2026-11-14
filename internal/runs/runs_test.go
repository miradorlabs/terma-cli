package runs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

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
