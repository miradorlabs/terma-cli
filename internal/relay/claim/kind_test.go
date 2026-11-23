package claim

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

func repoAt(origin string) config.Repository { return config.Repository{Origin: origin} }

// placed is a location-only placement: a kind and where, no project.
func placed(kind Kind, origin, cwd, run string, pids ...int) Claim {
	c := Claim{Kind: kind, Cwd: cwd, Run: run, PIDs: pids}
	if origin != "" {
		c.Repository = repoAt(origin)
	}
	return c
}

func mustRead(t *testing.T, dir, sid string, now time.Time) Claim {
	t.Helper()
	c, ok := Read(dir, sid, now)
	if !ok {
		t.Fatalf("no claim for %s", sid)
	}
	return c
}

// A placement with a kind may name a location alone, and the top level mirrors it; one
// with neither a project nor a kind is still refused.
func TestLocationOnlyPlacements(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	now := time.Now()
	if !Write(dir, "s", placed(KindOutsideGit, "", "/home/dev/scratch", "r1", 10), now) {
		t.Fatal("a location-only placement was refused")
	}
	c := mustRead(t, dir, "s", now)
	if c.Kind != KindOutsideGit || c.ProjectID != "" || c.Run != "r1" || len(c.Placements) != 1 || c.Placements[0].Kind != KindOutsideGit {
		t.Fatalf("claim %+v", c)
	}
	if Write(dir, "s", placed(KindOutsideGit, "", "/home/dev/scratch", "r1", 10), now) {
		t.Fatal("the same location within Refresh rewrote the claim")
	}
	if Write(dir, "s2", Claim{Tool: "codex"}, now) {
		t.Fatal("a placement with neither a project nor a kind was written")
	}
}

// samePlace compares the kind, the project and the repository, and outside git the
// directory: another directory outside git is another place, another directory in a
// checkout is the same one.
func TestSamePlaceComparesTheLocation(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	at := time.Now().Add(-time.Hour)
	steps := []struct {
		c    Claim
		want int
	}{
		{placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), 1},
		{placed(KindRepository, "github.com/acme/api", "/src/api/web", "r1", 10), 1},
		{placed(KindRepository, "github.com/acme/web", "/src/web", "r1", 10), 2},
		{placed(KindOutsideGit, "", "/tmp/a", "r1", 10), 3},
		{placed(KindOutsideGit, "", "/tmp/a", "r1", 10), 3},
		{placed(KindOutsideGit, "", "/tmp/b", "r1", 10), 4},
		{Claim{ProjectID: "p1", Repository: repoAt("github.com/acme/web"), Cwd: "/src/web", PIDs: []int{10}}, 5},
	}
	for i, step := range steps {
		at = at.Add(time.Minute)
		Write(dir, "s", step.c, at)
		if got := mustRead(t, dir, "s", at); len(got.Placements) != step.want || got.Kind != step.c.Kind || got.Cwd != step.c.Cwd {
			t.Fatalf("step %d: %d placements, kind %q, cwd %q; want %d, %q, %q", i, len(got.Placements), got.Kind, got.Cwd, step.want, step.c.Kind, step.c.Cwd)
		}
	}
}

// A Mark is never read as a run outside git: its placement keeps no kind and no project,
// and a location-only placement after it is another place.
func TestAMarkIsNotOutsideGit(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	t1 := t0.Add(10 * time.Minute)
	Mark(dir, "s", "codex", []int{10}, t0)
	if !Write(dir, "s", placed(KindOutsideGit, "", "/tmp/a", "r1", 10), t1) {
		t.Fatal("the placement after the mark was not written")
	}
	c := mustRead(t, dir, "s", t1)
	if len(c.Placements) != 2 {
		t.Fatalf("placements %+v", c.Placements)
	}
	if got, ok := c.At(10, t0.Add(time.Minute)); !ok || got.Kind != "" || got.ProjectID != "" {
		t.Fatalf("the mark's record = %+v, %v; want not collected", got, ok)
	}
	if got, ok := c.At(10, t1.Add(time.Minute)); !ok || got.Kind != KindOutsideGit || got.Cwd != "/tmp/a" {
		t.Fatalf("the later record = %+v, %v", got, ok)
	}
}

// Mixed only turns on: within its run no write of another kind replaces it, a write naming
// no run cannot tell and is refused too, and a new run starts afresh.
func TestMixedOnlyTurnsOn(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	at := time.Now().Add(-time.Hour)
	next := func() time.Time { at = at.Add(time.Minute); return at }
	Write(dir, "s", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), next())
	if !Write(dir, "s", placed(KindMixed, "github.com/acme/api", "/src/api", "r1", 10), next()) {
		t.Fatal("the crossing was not written")
	}
	for i, c := range []Claim{
		placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10),
		placed(KindOutsideGit, "", "/tmp/a", "r1", 10),
		placed(KindRepository, "github.com/acme/api", "/src/api", "", 10),
		{ProjectID: "p1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}},
	} {
		if Write(dir, "s", c, next()) {
			t.Fatalf("write %d replaced a Mixed placement in its run", i)
		}
	}
	if Mark(dir, "s", "claude-code", []int{10}, next()) {
		t.Fatal("a mark replaced a Mixed placement")
	}
	if c := mustRead(t, dir, "s", at); c.Kind != KindMixed || len(c.Placements) != 2 {
		t.Fatalf("claim %+v", c)
	}
	if !Write(dir, "s", placed(KindMixed, "github.com/acme/web", "/src/web", "r1", 11), next()) {
		t.Fatal("a Mixed write of the same run was refused")
	}
	if !Write(dir, "s", placed(KindRepository, "github.com/acme/api", "/src/api", "r2", 20), next()) {
		t.Fatal("a resume (a new run) did not replace the Mixed placement")
	}
	if c := mustRead(t, dir, "s", at); c.Kind != KindRepository || c.Run != "r2" {
		t.Fatalf("after the resume: %+v", c)
	}
}

// A write that could not take the lock never replaces a Mixed placement, even for a new
// run: it may have read the claim before a concurrent write made it Mixed. A Mixed write
// still lands without the lock.
func TestALockTimeoutNeverReplacesMixed(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	at := time.Now().Add(-time.Hour)
	Write(dir, "s", placed(KindMixed, "github.com/acme/api", "/src/api", "r1", 10), at)
	p, _ := path(dir, "s")
	unlock, err := flock.TryLock(p + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if Write(dir, "s", placed(KindRepository, "github.com/acme/api", "/src/api", "r2", 20), at.Add(time.Minute)) {
		t.Fatal("a write without the lock replaced a Mixed placement")
	}
	if !Write(dir, "s", placed(KindMixed, "github.com/acme/web", "/src/web", "r1", 10), at.Add(2*time.Minute)) {
		t.Fatal("a Mixed write without the lock did not land")
	}
	if c := mustRead(t, dir, "s", at); c.Kind != KindMixed {
		t.Fatalf("claim %+v", c)
	}
}

// writeSpread writes n placements in distinct repositories, a minute apart from t0, for
// pid 10, and returns the claim.
func writeSpread(t *testing.T, dir, sid string, t0 time.Time, n int, kinds bool) Claim {
	t.Helper()
	for i := range n {
		c := Claim{ProjectID: fmt.Sprintf("p%d", i), Repository: repoAt(fmt.Sprintf("github.com/acme/r%d", i)), PIDs: []int{10}}
		if kinds {
			c.Kind, c.Run = KindRepository, "r1"
		}
		if !Write(dir, sid, c, t0.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("placement %d not written", i)
		}
	}
	return mustRead(t, dir, sid, t0.Add(time.Duration(n)*time.Minute))
}

// The claim records that it dropped a placement. With kinds, a record older than every
// kept placement is then not covered, nor is one without a time; the records the kept
// placements cover still are. A legacy claim reads as it always has.
func TestEviction(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if c := writeSpread(t, dir, "full", t0, maxPlacements, true); c.Evicted {
		t.Fatal("a claim holding maxPlacements placements says it evicted one")
	}
	c := writeSpread(t, dir, "kinds", t0, maxPlacements+1, true)
	if !c.Evicted || len(c.Placements) != maxPlacements || c.Placements[0].ProjectID != "p1" {
		t.Fatalf("after one more: evicted %v, %d placements, first %q", c.Evicted, len(c.Placements), c.Placements[0].ProjectID)
	}
	if _, ok := c.At(10, t0.Add(30*time.Second)); ok {
		t.Fatal("a record of the evicted placement's time was covered")
	}
	if got, ok := c.At(10, t0.Add(90*time.Second)); !ok || got.ProjectID != "p1" || !got.Evicted {
		t.Fatalf("a record of a kept placement = %+v, %v", got, ok)
	}
	if _, ok := c.At(10, time.Time{}); ok {
		t.Fatal("a record without a time was covered after an eviction")
	}
	// The mark stays once set, through a write that evicts nothing.
	latest := Claim{ProjectID: fmt.Sprintf("p%d", maxPlacements), Kind: KindRepository, Run: "r1", Repository: repoAt(fmt.Sprintf("github.com/acme/r%d", maxPlacements)), PIDs: []int{11}}
	if !Write(dir, "kinds", latest, t0.Add(time.Hour)) {
		t.Fatal("a new process in the latest placement was not written")
	}
	if c := mustRead(t, dir, "kinds", t0.Add(time.Hour)); !c.Evicted || len(c.Placements) != maxPlacements {
		t.Fatalf("a later write cleared Evicted: %v, %d placements", c.Evicted, len(c.Placements))
	}

	legacy := writeSpread(t, dir, "legacy", t0, maxPlacements+1, false)
	if !legacy.Evicted {
		t.Fatal("a legacy claim did not record its eviction")
	}
	if got, ok := legacy.At(10, t0.Add(30*time.Second)); !ok || got.ProjectID != "p1" {
		t.Fatalf("a legacy claim's old record = %+v, %v; want the earliest kept, as before", got, ok)
	}
	if got, ok := legacy.At(10, time.Time{}); !ok || got.ProjectID != fmt.Sprintf("p%d", maxPlacements) {
		t.Fatalf("a legacy claim's untimed record = %+v, %v; want the latest, as before", got, ok)
	}
}

// The first export can race the session's first hook: with nothing evicted, a record older
// than every placement takes the earliest covering one.
func TestFirstExportRacesItsHook(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	Write(dir, "s", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0)
	Write(dir, "s", placed(KindOutsideGit, "", "/tmp/a", "r1", 10), t0.Add(time.Minute))
	c := mustRead(t, dir, "s", t0)
	if got, ok := c.At(10, t0.Add(-time.Second)); !ok || got.Kind != KindRepository {
		t.Fatalf("the racing export = %+v, %v", got, ok)
	}
	// Another run's first records, in its own process, are its own too.
	Write(dir, "s", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 20), t0.Add(2*time.Minute))
	c = mustRead(t, dir, "s", t0)
	if got, ok := c.At(20, t0.Add(30*time.Second)); !ok || got.Run != "r2" {
		t.Fatalf("the second run's early record = %+v, %v", got, ok)
	}
}

// A record without a time uses the latest placement only if nothing was evicted and every
// placement covering it has one Kind and Repository.
func TestUntimestampedRecords(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	Write(dir, "outside", placed(KindOutsideGit, "", "/tmp/a", "r1", 10), t0)
	Write(dir, "outside", placed(KindOutsideGit, "", "/tmp/b", "r1", 10), t0.Add(time.Minute))
	if got, ok := mustRead(t, dir, "outside", t0).At(10, time.Time{}); !ok || got.Cwd != "/tmp/b" {
		t.Fatalf("two directories outside git = %+v, %v; want the latest", got, ok)
	}

	Write(dir, "moved", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0)
	Write(dir, "moved", placed(KindOutsideGit, "", "/tmp/a", "r1", 10), t0.Add(time.Minute))
	if got, ok := mustRead(t, dir, "moved", t0).At(10, time.Time{}); ok {
		t.Fatalf("a record without a time across two locations = %+v", got)
	}

	// Two runs in their own processes: a record's process alone places it.
	Write(dir, "runs", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0)
	Write(dir, "runs", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 20), t0.Add(time.Minute))
	c := mustRead(t, dir, "runs", t0)
	if got, ok := c.At(10, time.Time{}); !ok || got.Run != "r1" {
		t.Fatalf("the first run's untimed record = %+v, %v", got, ok)
	}
	if _, ok := c.At(0, time.Time{}); ok {
		t.Fatal("an unidentified sender was covered")
	}
}

// A claim written as before kinds carries none of the new fields, byte for byte.
func TestLegacyClaimsStayAsTheyWere(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	now := time.Now()
	Write(dir, "s", Claim{ProjectID: "p1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, now)
	Mark(dir, "m", "codex", []int{10}, now)
	for _, sid := range []string{"s", "m"} {
		p, _ := path(dir, sid)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"kind"`, `"run"`, `"evicted"`} {
			if strings.Contains(string(data), field) {
				t.Errorf("%s holds %s: %s", sid, field, data)
			}
		}
	}
}

// A placement names the profile its hook routed under, and one under another profile is
// another place: a record made before the switch keeps the profile it was placed under, one
// made after takes the new one.
func TestAPlacementNamesItsProfile(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	before := time.Now().Add(-time.Minute)
	work := placed(KindRepository, "github.com/acme/api", "", "r1", 10)
	work.Profile = "work"
	if !Write(dir, "s", work, before) {
		t.Fatal("the first placement was not written")
	}
	client := work
	client.Profile = "client"
	if !Write(dir, "s", client, before.Add(30*time.Second)) {
		t.Fatal("a placement under another profile was taken for the same place")
	}
	c := mustRead(t, dir, "s", time.Now())
	if c.Profile != "client" || len(c.Placements) != 2 || c.Placements[0].Profile != "work" {
		t.Fatalf("claim %+v", c)
	}
	for at, want := range map[time.Time]string{before.Add(10 * time.Second): "work", time.Now(): "client"} {
		if got, ok := c.At(10, at); !ok || got.Profile != want {
			t.Errorf("At(%v) = %+v, want profile %s", at, got, want)
		}
	}
	// An untimestamped record spanning a profile switch is not covered.
	if got, ok := c.At(10, time.Time{}); ok {
		t.Fatalf("untimed record across profile switch was covered: %+v", got)
	}
}

// Location-only claims preserve #152's Account lane and subagent Parent across Write/At,
// separate placements made under different accounts, and keep CatchAll separate from
// Profile (never persisted to JSON).
func TestKindClaimPreservesAccountParentAndCatchAll(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	alice := placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10)
	alice.Account, alice.Profile, alice.Parent, alice.CatchAll = "aaaaaaaaaaaaaaaa", "default", "parent-sess", true
	if !Write(dir, "s", alice, t0) {
		t.Fatal("alice placement was not written")
	}

	// Same repo under Bob's account is a distinct placement; blank Parent keeps prev.Parent.
	bob := placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10)
	bob.Account, bob.Profile = "bbbbbbbbbbbbbbbb", "default"
	if !Write(dir, "s", bob, t0.Add(time.Minute)) {
		t.Fatal("bob placement was not written")
	}

	c := mustRead(t, dir, "s", t0.Add(2*time.Minute))
	if c.CatchAll {
		t.Fatal("CatchAll must never persist to disk")
	}
	if len(c.Placements) != 2 || c.Parent != "parent-sess" {
		t.Fatalf("claim = %+v, want 2 placements and preserved Parent", c)
	}
	if got, ok := c.At(10, t0.Add(30*time.Second)); !ok || got.Account != "aaaaaaaaaaaaaaaa" || got.Profile != "default" || got.Parent != "parent-sess" {
		t.Fatalf("alice record At = %+v, %v", got, ok)
	}
	if got, ok := c.At(10, t0.Add(2*time.Minute)); !ok || got.Account != "bbbbbbbbbbbbbbbb" || got.Profile != "default" || got.Parent != "parent-sess" {
		t.Fatalf("bob record At = %+v, %v", got, ok)
	}
	// An untimestamped record spanning an account switch is refused.
	if got, ok := c.At(10, time.Time{}); ok {
		t.Fatalf("untimed record across account switch was covered: %+v", got)
	}
}

// KindMixed never retains a ProjectID, unlocked non-Mixed Kind writes back off instead of
// risking an overwrite of a concurrent Mixed write, and resuming in the same repository
// under a new run starts a distinct placement.
func TestKindMixedStripsProjectAndSameRepoResumeSeparatesRuns(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	// KindMixed strips any caller-supplied ProjectID.
	mixed := placed(KindMixed, "github.com/acme/api", "/src/api", "r1", 10)
	mixed.ProjectID = "p-should-clear"
	if !Write(dir, "m", mixed, t0) {
		t.Fatal("Mixed write failed")
	}
	if got := mustRead(t, dir, "m", t0); got.Kind != KindMixed || got.ProjectID != "" {
		t.Fatalf("Mixed claim kept ProjectID: %+v", got)
	}

	// Resume in the same repository under a new run creates a distinct placement.
	if !Write(dir, "resume", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0) {
		t.Fatal("first run write failed")
	}
	if !Write(dir, "resume", placed(KindRepository, "github.com/acme/api", "/src/api", "r2", 10), t0.Add(10*time.Minute)) {
		t.Fatal("resumed run write failed")
	}
	c := mustRead(t, dir, "resume", t0.Add(10*time.Minute))
	if len(c.Placements) != 2 || c.Placements[0].Run != "r1" || c.Placements[1].Run != "r2" {
		t.Fatalf("placements = %+v, want r1 then r2", c.Placements)
	}
	if got, ok := c.At(10, t0.Add(time.Minute)); !ok || got.Run != "r1" {
		t.Fatalf("earlier record At = %+v, %v; want r1", got, ok)
	}
	if got, ok := c.At(10, t0.Add(11*time.Minute)); !ok || got.Run != "r2" {
		t.Fatalf("later record At = %+v, %v; want r2", got, ok)
	}
	if got, ok := c.At(10, time.Time{}); ok {
		t.Fatalf("untimed record spanning r1 and r2 was covered: %+v", got)
	}

	// Any non-Mixed write (kinded or legacy) backs off while the lock is held.
	p, _ := path(dir, "resume")
	unlock, err := flock.TryLock(p + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if Write(dir, "resume", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 10), t0.Add(15*time.Minute)) {
		t.Fatal("unlocked non-Mixed Kind write proceeded while lock was held")
	}
	if Write(dir, "resume", Claim{ProjectID: "p1", Repository: repoAt("github.com/acme/web"), PIDs: []int{10}}, t0.Add(16*time.Minute)) {
		t.Fatal("unlocked legacy write proceeded while lock was held")
	}

	// Untimestamped record spanning a Mark (no kind) and a kinded placement is refused.
	Mark(dir, "mixed-mark", "codex", []int{10}, t0)
	if !Write(dir, "mixed-mark", placed(KindOutsideGit, "", "/tmp/a", "r1", 10), t0.Add(time.Minute)) {
		t.Fatal("kinded placement after Mark was not written")
	}
	if got, ok := mustRead(t, dir, "mixed-mark", t0.Add(time.Minute)).At(10, time.Time{}); ok {
		t.Fatalf("untimed record spanning Mark and KindOutsideGit was covered: %+v", got)
	}
}

// A KindMixed write whose lock timed out lands in the lock-free .mixed sidecar, survives a
// slow non-Mixed lock holder overwriting the main claim file with stale bytes, folds into
// the main file on the next locked write (which still refuses to clear Mixed), and expires
// on Prune.
func TestMixedAsideSurvivesStaleLockHolderAndPrunes(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	now := time.Now().UTC().Truncate(time.Second)

	if !Write(dir, "race", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
		t.Fatal("initial claim write failed")
	}
	p, ok := path(dir, "race")
	if !ok {
		t.Fatal("claim path rejected")
	}
	staleBytes, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a slow non-Mixed writer holding the lock after reading pre-Mixed state.
	unlock, err := flock.TryLock(p + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if !Write(dir, "race", placed(KindMixed, "github.com/acme/web", "/src/web", "r1", 11), now.Add(time.Minute)) {
		unlock()
		t.Fatal("lock-timeout Mixed write failed")
	}
	// Slow lock holder finishes writing stale non-Mixed claim and releases lock.
	if err := os.WriteFile(p, staleBytes, 0o600); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()

	// Read must still observe KindMixed via the sidecar.
	got := mustRead(t, dir, "race", now.Add(2*time.Minute))
	if got.Kind != KindMixed || got.ProjectID != "" || got.Run != "r1" {
		t.Fatalf("Read after stale overwrite = %+v, want KindMixed for r1", got)
	}

	// Next locked non-Mixed write folds the sidecar into p, cleans .mixed, and refuses downgrade.
	if Write(dir, "race", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(3*time.Minute)) {
		t.Fatal("locked non-Mixed write downgraded folded KindMixed claim")
	}
	if _, err := os.Stat(asidePath(p)); !os.IsNotExist(err) {
		t.Fatalf("sidecar dir %s still exists after fold: err=%v", asidePath(p), err)
	}
	if got := mustRead(t, dir, "race", now.Add(3*time.Minute)); got.Kind != KindMixed || got.Run != "r1" {
		t.Fatalf("Read after fold = %+v, want KindMixed for r1", got)
	}

	// Prune cleans expired .mixed sidecar entries and removes empty sidecar directories.
	p2, _ := path(dir, "stale-aside")
	if !writeMixedAside(p2, placed(KindMixed, "", "", "r1", 20), now.Add(-TTL-time.Hour)) {
		t.Fatal("writeMixedAside failed")
	}
	asideDir := asidePath(p2)
	entries, err := os.ReadDir(asideDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("aside entries = %v, err = %v", entries, err)
	}
	old := now.Add(-TTL - time.Hour)
	if err := os.Chtimes(filepath.Join(asideDir, entries[0].Name()), old, old); err != nil {
		t.Fatal(err)
	}
	Prune(dir, now)
	if _, err := os.Stat(asideDir); !os.IsNotExist(err) {
		t.Fatalf("expired sidecar dir %s survived Prune: err=%v", asideDir, err)
	}
}

// Sidecar edge cases: expired marks never resurrect Mixed on locked writes, earlier-run
// marks survive after a newer run landed, marks under another account never merge PIDs,
// duplicate marks dedupe, pending marks bypass the fast path, and concurrent Reads during a
// fold always observe Mixed.
func TestMixedAsideEdgeCasesAndConcurrentRead(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	now := time.Now().UTC().Truncate(time.Second)

	// 1. Expired sidecar mark must not resurrect Mixed on the next locked write.
	pExp, _ := path(dir, "expired-aside")
	if !writeMixedAside(pExp, placed(KindMixed, "", "", "r1", 10), now.Add(-TTL-time.Hour)) {
		t.Fatal("writeMixedAside failed")
	}
	entries, err := os.ReadDir(asidePath(pExp))
	if err != nil || len(entries) != 1 {
		t.Fatalf("aside entries = %v, err = %v", entries, err)
	}
	old := now.Add(-TTL - time.Hour)
	if err := os.Chtimes(filepath.Join(asidePath(pExp), entries[0].Name()), old, old); err != nil {
		t.Fatal(err)
	}
	if !Write(dir, "expired-aside", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
		t.Fatal("locked write blocked by expired sidecar mark")
	}
	if got := mustRead(t, dir, "expired-aside", now); got.Kind != KindRepository {
		t.Fatalf("Read after expired sidecar = %+v, want KindRepository", got)
	}
	if _, err := os.Stat(asidePath(pExp)); !os.IsNotExist(err) {
		t.Fatalf("expired sidecar dir still exists after locked write: err=%v", err)
	}

	// 2. Older run's Mixed mark folded after r2 landed keeps r1 Mixed and r2 intact.
	if !Write(dir, "older-run", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(-10*time.Minute)) {
		t.Fatal("r1 write failed")
	}
	if !Write(dir, "older-run", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 20), now.Add(-2*time.Minute)) {
		t.Fatal("r2 write failed")
	}
	pOld, _ := path(dir, "older-run")
	if !writeMixedAside(pOld, placed(KindMixed, "", "", "r1", 10), now.Add(-5*time.Minute)) {
		t.Fatal("r1 sidecar mark failed")
	}
	// Locked write for r2 folds r1's delayed mark into Placements while keeping r2 live.
	if !Write(dir, "older-run", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 21), now) {
		t.Fatal("r2 locked write failed")
	}
	cOld := mustRead(t, dir, "older-run", now)
	if cOld.Kind != KindRepository || cOld.Run != "r2" {
		t.Fatalf("latest run = %+v, want KindRepository for r2", cOld)
	}
	if got, ok := cOld.At(10, now.Add(-4*time.Minute)); !ok || got.Kind != KindMixed || got.Run != "r1" {
		t.Fatalf("r1 after crossing = %+v, %v; want KindMixed", got, ok)
	}

	// 3. Cross-account sidecar mark must not merge prev's PIDs into the new account lane.
	alice := placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10)
	alice.Account, alice.Profile = "aaaaaaaaaaaaaaaa", "work"
	if !Write(dir, "cross-acct", alice, now.Add(-2*time.Minute)) {
		t.Fatal("alice write failed")
	}
	pAcct, _ := path(dir, "cross-acct")
	bob := placed(KindMixed, "", "", "r1", 20)
	bob.Account, bob.Profile = "bbbbbbbbbbbbbbbb", "work"
	if !writeMixedAside(pAcct, bob, now.Add(-time.Minute)) {
		t.Fatal("bob sidecar write failed")
	}
	// Duplicate bob mark with same PID dedupes to a single sidecar file.
	if !writeMixedAside(pAcct, bob, now) {
		t.Fatal("bob duplicate sidecar write failed")
	}
	if es, err := os.ReadDir(asidePath(pAcct)); err != nil || len(es) != 1 {
		t.Fatalf("sidecar dedupe failed: len=%d, err=%v", len(es), err)
	}
	gotAcct := mustRead(t, dir, "cross-acct", now)
	if gotAcct.Kind != KindMixed || gotAcct.Account != "bbbbbbbbbbbbbbbb" || slices.Contains(gotAcct.PIDs, 10) {
		t.Fatalf("cross-account fold leaked PIDs: %+v", gotAcct)
	}

	// 4. Fast path must not short-circuit while a sidecar mark is pending; concurrent
	// Readers during fold always see KindMixed.
	if !Write(dir, "conc", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
		t.Fatal("conc initial write failed")
	}
	pConc, _ := path(dir, "conc")
	if !writeMixedAside(pConc, placed(KindMixed, "", "", "r1", 10), now.Add(time.Second)) {
		t.Fatal("conc sidecar write failed")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if c, ok := Read(dir, "conc", now.Add(2*time.Second)); !ok || c.Kind != KindMixed {
						t.Errorf("concurrent Read during fold saw ok=%v kind=%q", ok, c.Kind)
						return
					}
				}
			}
		})
	}
	// Same-place write within Refresh still enters lock because aside is pending, folds it, and returns false.
	if Write(dir, "conc", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(2*time.Second)) {
		close(stop)
		wg.Wait()
		t.Fatal("fast path downgraded pending sidecar mark")
	}
	close(stop)
	wg.Wait()
	if _, err := os.Stat(asidePath(pConc)); !os.IsNotExist(err) {
		t.Fatalf("conc sidecar dir not cleaned after fold: err=%v", err)
	}

	// 5. Out-of-order Since race: a same-run non-Mixed placement written at t10.1 after a
	// lock-timeout Mixed mark stamped t10 must still resolve as KindMixed at t10.05 and t11.
	t9 := now.Add(-10 * time.Second)
	t10 := now.Add(-5 * time.Second)
	t101 := now.Add(-4 * time.Second)
	if !Write(dir, "since-race", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t9) {
		t.Fatal("since-race initial write failed")
	}
	if !Write(dir, "since-race", placed(KindOutsideGit, "", "/tmp/work", "r1", 10), t101) {
		t.Fatal("since-race outside-git write failed")
	}
	pRace, _ := path(dir, "since-race")
	if !writeMixedAside(pRace, placed(KindMixed, "", "", "r1", 10), t10) {
		t.Fatal("since-race sidecar write failed")
	}
	cRace := mustRead(t, dir, "since-race", now)
	for _, at := range []time.Time{t101, now} {
		if got, ok := cRace.At(10, at); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("At(%v) = %+v, %v; want KindMixed", at, got, ok)
		}
	}

	// 6. Corrupt unexpired sidecar over an existing non-Mixed claim fails closed for existing PIDs.
	if !Write(dir, "corrupt-existing", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t9) {
		t.Fatal("corrupt-existing initial write failed")
	}
	pCorrupt, _ := path(dir, "corrupt-existing")
	if err := os.MkdirAll(asidePath(pCorrupt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asidePath(pCorrupt), "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Locked write folds the corrupt sidecar and refuses to downgrade.
	if Write(dir, "corrupt-existing", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
		t.Fatal("corrupt sidecar did not block downgrade")
	}
	if got, ok := mustRead(t, dir, "corrupt-existing", now).At(10, now); !ok || got.Kind != KindMixed || got.ProjectID != "" {
		t.Fatalf("At after corrupt sidecar fold = %+v, %v; want KindMixed", got, ok)
	}

	// 7. Live sidecar survives an expired main claim; earlier-run mark poisons post-crossing
	// placements before r2; unseen new-run mark with earlier timestamp is never dropped.
	if !Write(dir, "exp-main", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(-TTL-time.Hour)) {
		t.Fatal("exp-main write failed")
	}
	pExpMain, _ := path(dir, "exp-main")
	if err := os.Chtimes(pExpMain, old, old); err != nil {
		t.Fatal(err)
	}
	if !writeMixedAside(pExpMain, placed(KindMixed, "", "", "r1", 10), now) {
		t.Fatal("exp-main sidecar failed")
	}
	if got := mustRead(t, dir, "exp-main", now); got.Kind != KindMixed {
		t.Fatalf("live sidecar with expired main = %+v, want KindMixed", got)
	}

	// Earlier-run post-crossing placement poisoned before r2.
	if !Write(dir, "early-poison", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(-10*time.Minute)) {
		t.Fatal("r1 repo write failed")
	}
	if !Write(dir, "early-poison", placed(KindOutsideGit, "", "/tmp/work", "r1", 10), now.Add(-6*time.Minute)) {
		t.Fatal("r1 outside-git write failed")
	}
	if !Write(dir, "early-poison", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 20), now.Add(-2*time.Minute)) {
		t.Fatal("r2 write failed")
	}
	pEP, _ := path(dir, "early-poison")
	if !writeMixedAside(pEP, placed(KindMixed, "", "", "r1", 10), now.Add(-8*time.Minute)) {
		t.Fatal("r1 delayed sidecar failed")
	}
	cEP := mustRead(t, dir, "early-poison", now)
	if got, ok := cEP.At(10, now.Add(-5*time.Minute)); !ok || got.Kind != KindMixed {
		t.Fatalf("r1 after t1 crossing = %+v, %v; want KindMixed", got, ok)
	}
	if got, ok := cEP.At(20, now); !ok || got.Kind != KindRepository || got.Run != "r2" {
		t.Fatalf("r2 after r1 fold = %+v, %v; want KindRepository r2", got, ok)
	}

	// Unseen new-run mark stamped earlier than r1's ClaimedAt is appended, never dropped.
	if !Write(dir, "skew-r2", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
		t.Fatal("skew-r2 r1 write failed")
	}
	pSkew, _ := path(dir, "skew-r2")
	if !writeMixedAside(pSkew, placed(KindMixed, "", "", "r2", 20), now.Add(-time.Second)) {
		t.Fatal("skew-r2 sidecar failed")
	}
	if got := mustRead(t, dir, "skew-r2", now); got.Kind != KindMixed || got.Run != "r2" {
		t.Fatalf("unseen r2 mark = %+v, want KindMixed r2", got)
	}
}
