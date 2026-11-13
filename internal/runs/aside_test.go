package runs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// held holds sid's ledger lock until the test ends, so no ledger write can be made.
func held(t *testing.T, dir, sid string) func() {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(dir, Dir), 0o700))
	unlock, err := flock.TryLock(filepath.Join(dir, Dir, sid+".json.lock"))
	must(t, err)
	t.Cleanup(unlock)
	return unlock
}

// A crossing recorded aside, its ledger's lock held, reads as Mixed from its time to
// every reader, as a crossing in the ledger would: MixedAt for the session and its
// subagents, Read, Locate and doctor's list. The run in force before it is not.
func TestACrossingRecordedAsideReadsMixed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := start(t, dir, "s", t0)
	must(t, Alias(dir, "s", "agent-1"))
	run := start(t, dir, "s", t0.Add(time.Hour))
	held(t, dir, "s")
	crossed := t0.Add(2 * time.Hour)
	must(t, MarkAside(dir, "s", run, crossed))
	if err := MarkMixed(dir, "s", crossed); err == nil {
		t.Fatal("the ledger took a write through its held lock")
	}

	for _, id := range []string{"s", "agent-1"} {
		if at, mixed := mixedAt(t, dir, id, crossed.Add(time.Minute)); !mixed || !at.Equal(crossed) {
			t.Errorf("%s after the crossing: Mixed %v from %v", id, mixed, at)
		}
	}
	if at, mixed := Read(dir, "s").MixedAt(time.Time{}); !mixed || !at.Equal(crossed) {
		t.Errorf("Read: Mixed %v from %v", mixed, at)
	}
	if _, mixed := mixedAt(t, dir, "s", t0.Add(time.Minute)); mixed {
		t.Errorf("the run %s before the crossing reads Mixed", first)
	}
	p, ok := Locate(dir, "s")
	if !ok || p.Run != run || !p.MixedAt.Equal(crossed) || !p.Aside || !slices.Equal(p.Aliases, []string{"agent-1"}) {
		t.Errorf("Locate = %+v, %v", p, ok)
	}
	if active, _ := Mixed(dir, crossed.Add(time.Hour), time.Hour*24); !slices.Equal(active, []string{"s"}) {
		t.Errorf("doctor lists %v", active)
	}
}

// A sidecar alone, with no ledger behind it, is a run that crossed and covers every
// earlier record, as MarkMixed with no run recorded would start.
func TestASidecarWithNoLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	held(t, dir, "s")
	must(t, MarkAside(dir, "s", "", t0))
	if at, mixed := mixedAt(t, dir, "s", t0.Add(-time.Hour)); !mixed || !at.Equal(t0) {
		t.Errorf("Mixed %v from %v", mixed, at)
	}
	if p, ok := Locate(dir, "s"); !ok || p.Run == "" || !p.MixedAt.Equal(t0) {
		t.Errorf("Locate = %+v, %v", p, ok)
	}
}

// The first edit that holds the ledger's lock folds the sidecar in, an earlier mark
// winning, and removes it; the run stays Mixed from the earliest crossing. A new run
// begun by that edit is not Mixed.
func TestTheNextLockedEditFoldsTheSidecarIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := start(t, dir, "s", t0)
	unlock := held(t, dir, "s")
	must(t, MarkAside(dir, "s", run, t0.Add(2*time.Hour)))
	must(t, MarkAside(dir, "s", run, t0.Add(time.Hour)))
	must(t, MarkAside(dir, "s", run, t0.Add(3*time.Hour))) // later: changes nothing
	if _, err := TryTouch(dir, "s", run, []string{"github.com/acme/api"}); err == nil {
		t.Fatal("TryTouch took a held lock")
	}
	unlock()
	if _, err := TryTouch(dir, "s", run, nil); err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(dir, Dir, "s.mixed")
	if !gone(side) {
		t.Fatal("the fold left the sidecar")
	}
	if at, mixed := mixedAt(t, dir, "s", time.Time{}); !mixed || !at.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after the fold: Mixed %v from %v", mixed, at)
	}
	if p, _ := Locate(dir, "s"); p.Aside {
		t.Error("Locate still finds a sidecar")
	}
	must(t, MarkAside(dir, "s", run, t0.Add(30*time.Minute)))
	start(t, dir, "s", t0.Add(4*time.Hour))
	if !gone(side) {
		t.Fatal("a run's start left the sidecar")
	}
	if at, mixed := mixedAt(t, dir, "s", t0.Add(2*time.Hour)); !mixed || !at.Equal(t0.Add(30*time.Minute)) {
		t.Errorf("the crossed run: Mixed %v from %v", mixed, at)
	}
	if _, mixed := mixedAt(t, dir, "s", time.Time{}); mixed {
		t.Error("the new run reads Mixed")
	}
}

// A fold never removes a mark it did not read: one written as folds run is kept, in the
// ledger or in the sidecar, and so are the marks of two writers that both miss the lock.
func TestAFoldKeepsEveryMark(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := start(t, dir, "s", t0)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			if i%4 == 0 {
				_, _ = TryTouch(dir, "s", run, nil)
				return
			}
			must(t, MarkAside(dir, "s", run, t0.Add(time.Duration(i)*time.Minute)))
		})
	}
	wg.Wait()
	if at, mixed := mixedAt(t, dir, "s", time.Time{}); !mixed || !at.Equal(t0.Add(time.Minute)) {
		t.Errorf("before the last fold: Mixed %v from %v, want from the earliest mark", mixed, at)
	}
	must(t, MarkMixed(dir, "s", t0.Add(5*time.Hour)))
	if !gone(filepath.Join(dir, Dir, "s.mixed")) {
		t.Fatal("a locked edit left the sidecar")
	}
	if at, mixed := mixedAt(t, dir, "s", time.Time{}); !mixed || !at.Equal(t0.Add(time.Minute)) {
		t.Errorf("Mixed %v from %v, want from the earliest mark", mixed, at)
	}
}

// Writers that all miss the ledger's lock keep every mark.
func TestConcurrentMarksAside(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start(t, dir, "s", t0)
	held(t, dir, "s")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() { must(t, MarkAside(dir, "s", "run-"+string(rune('a'+i)), t0.Add(time.Duration(i)*time.Minute))) })
	}
	wg.Wait()
	a, err := loadAside(filepath.Join(dir, Dir, "s.mixed"))
	must(t, err)
	if len(a.marks) != 16 {
		t.Errorf("%d of 16 marks kept: %+v", len(a.marks), a.marks)
	}
}

// Prune never removes a sidecar while it keeps its ledger, and keeps that ledger as long
// as a Mixed run's. A sidecar whose ledger is gone stays MixedRetention past its last mark.
func TestPruneKeepsASidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runs := filepath.Join(dir, Dir)
	long := time.Now().Add(-Retention - 24*time.Hour)
	run := start(t, dir, "kept", long)
	held(t, dir, "kept")()
	must(t, MarkAside(dir, "kept", run, long))
	must(t, MarkAside(dir, "alone", "", long))
	must(t, MarkAside(dir, "expired", "", long.Add(-MixedRetention)))
	for _, f := range []string{"kept.json", "kept.mixed", "alone.mixed"} {
		age(t, filepath.Join(runs, f), Retention+24*time.Hour)
	}
	age(t, filepath.Join(runs, "expired.mixed"), MixedRetention+Retention)

	Prune(context.Background(), dir, time.Now(), nil)
	for _, f := range []string{"kept.json", "kept.mixed", "alone.mixed"} {
		if gone(filepath.Join(runs, f)) {
			t.Errorf("%s was pruned", f)
		}
	}
	if !gone(filepath.Join(runs, "expired.mixed")) {
		t.Error("a sidecar past MixedRetention, with no ledger, survived")
	}
	Prune(context.Background(), dir, time.Now().Add(MixedRetention), map[string]bool{"alone": true})
	if gone(filepath.Join(runs, "alone.mixed")) {
		t.Error("a sidecar a queued record names was pruned")
	}
	Prune(context.Background(), dir, time.Now().Add(MixedRetention), nil)
	for _, f := range []string{"kept.json", "kept.mixed", "alone.mixed"} {
		if !gone(filepath.Join(runs, f)) {
			t.Errorf("%s survived MixedRetention", f)
		}
	}
}
