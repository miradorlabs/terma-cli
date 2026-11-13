package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// countingTeams is a team router of personal alone, which takes every unplaced record, that
// counts its reads of each session's run ledger.
func countingTeams(t *testing.T) (*Teams, string, func(session string) int) {
	t.Helper()
	cfg := migratedMachine(t)
	selectTeams(t, cfg, true, func(s *routing.State) { s.Deselect(s.Teams[0].EnrollmentID) })
	teams := NewTeams(cfg, TeamsDeps{})
	var mu sync.Mutex
	reads := map[string]int{}
	teams.readLedger = func(stateDir, id string) runs.Snapshot {
		mu.Lock()
		reads[id]++
		mu.Unlock()
		return runs.Read(stateDir, id)
	}
	return teams, cfg.StateDir, func(session string) int {
		mu.Lock()
		defer mu.Unlock()
		return reads[session]
	}
}

// Route reads a session's run ledger once per ledgerTTL, however many of its parts arrive.
// A crossing recorded meanwhile is seen once the read is ledgerTTL old, never later, and a
// part Route queued before then is still dropped by the recheck before it leaves.
func TestRouteReadsARunLedgerOncePerWindow(t *testing.T) {
	t.Parallel()
	teams, stateDir, reads := countingTeams(t)
	began := time.Now().Add(-time.Hour)
	if _, err := runs.Start(stateDir, "s1", began); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	var queued relay.Queued
	for range 100 {
		got := teams.Route(relay.Record{Session: "s1", At: at})
		if got.Outcome != relay.Send {
			t.Fatalf("a part of a run not Mixed: %+v", got)
		}
		queued = got.Queued
	}
	if n := reads("s1"); n != 1 {
		t.Fatalf("100 parts read the ledger %d times", n)
	}
	if err := runs.MarkMixed(stateDir, "s1", at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if v := teams.Recheck(queued.Enrollment).Entry(queued); v.Verdict != relay.Discard || v.Why != relay.WhyMixed {
		t.Fatalf("the recheck of a part queued before the crossing was recorded: %+v", v)
	}
	// The read Route trusts ages out ledgerTTL after it began, however often it is used.
	teams.ledgerMu.Lock()
	e := teams.ledgers["s1"]
	e.read = e.read.Add(-teams.ledgerTTL)
	teams.ledgers["s1"] = e
	teams.ledgerMu.Unlock()
	if got := teams.Route(relay.Record{Session: "s1", At: at}); got.Outcome != relay.Drop || got.Why != relay.WhyMixed {
		t.Fatalf("a part of a run Mixed a window ago: %+v", got)
	}
	if n := reads("s1"); n != 2 {
		t.Fatalf("the ledger was read %d times, want once more after the window", n)
	}
}

// A slow read never replaces a newer one: Route keeps what the read begun last found, so a
// run a later read found Mixed never reads as not Mixed again within the window.
func TestASlowLedgerReadNeverReplacesANewerOne(t *testing.T) {
	t.Parallel()
	teams, stateDir, _ := countingTeams(t)
	if _, err := runs.Start(stateDir, "s1", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	inRead, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	first := true
	teams.readLedger = func(stateDir, id string) runs.Snapshot {
		mu.Lock()
		slow := first
		first = false
		mu.Unlock()
		l := runs.Read(stateDir, id)
		if slow {
			close(inRead)
			<-release
		}
		return l
	}
	done := make(chan bool)
	go func() {
		_, mixed := teams.mixedAt("s1", time.Now())
		done <- mixed
	}()
	<-inRead // the slow read found the run not Mixed
	if err := runs.MarkMixed(stateDir, "s1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, mixed := teams.mixedAt("s1", time.Now()); !mixed {
		t.Fatal("a read begun after the crossing was recorded did not see it")
	}
	close(release)
	if <-done {
		t.Fatal("the slow read, begun before the crossing, saw it")
	}
	if _, mixed := teams.mixedAt("s1", time.Now()); !mixed {
		t.Fatal("the slow read replaced the newer one: the run reads as not Mixed")
	}
}

// A full cache of reads still fresh drops its oldest quarter, not everything: the sessions
// read last stay cached.
func TestAFullLedgerCacheDropsItsOldest(t *testing.T) {
	t.Parallel()
	teams, _, _ := countingTeams(t)
	base := time.Now()
	for i := range maxLedgers {
		teams.remember(fmt.Sprint("s", i), cachedLedger{read: base.Add(time.Duration(i) * time.Microsecond)})
	}
	teams.remember("newest", cachedLedger{read: base.Add(time.Millisecond)})
	if n := len(teams.ledgers); n < maxLedgers/2 || n > maxLedgers {
		t.Fatalf("the cache keeps %d of %d", n, maxLedgers)
	}
	for _, kept := range []string{"newest", fmt.Sprint("s", maxLedgers-1)} {
		if _, ok := teams.ledgers[kept]; !ok {
			t.Errorf("%s, read last, was dropped", kept)
		}
	}
	if _, ok := teams.ledgers["s0"]; ok {
		t.Error("the oldest read was kept")
	}
}
