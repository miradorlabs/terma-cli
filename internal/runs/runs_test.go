package runs

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	must(t, MarkMixed(dir, "sess", crossed))

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
	must(t, MarkMixed(dir, "sess", t0.Add(10*time.Minute)))
	must(t, MarkMixed(dir, "sess", t0.Add(20*time.Minute)))
	if got, _ := mixedAt(t, dir, "sess", t0.Add(time.Hour)); !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("a later crossing moved the time to %v", got)
	}
	must(t, MarkMixed(dir, "sess", t0.Add(5*time.Minute)))
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
	must(t, MarkMixed(dir, "sess", t0))
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
	must(t, MarkMixed(dir, "sess", t0.Add(time.Minute)))
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
		if err := MarkMixed(dir, bad, t0); err == nil {
			t.Errorf("MarkMixed(%q) succeeded", bad)
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
	must(t, MarkMixed(dir, "sess", t0.Add(time.Hour)))
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
