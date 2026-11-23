package claim

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if Write(dir, "s2", Claim{ProjectID: "p1", Kind: KindRepository, Run: "r1"}, now) {
		t.Fatal("a project claim without an account was written")
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
		{Claim{ProjectID: "p1", Account: testAccount, Repository: repoAt("github.com/acme/web"), Cwd: "/src/web", PIDs: []int{10}}, 5},
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
		{ProjectID: "p1", Account: testAccount, Repository: repoAt("github.com/acme/api"), PIDs: []int{10}},
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
		c := Claim{ProjectID: fmt.Sprintf("p%d", i), Account: testAccount, Repository: repoAt(fmt.Sprintf("github.com/acme/r%d", i)), PIDs: []int{10}}
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
	latest := Claim{ProjectID: fmt.Sprintf("p%d", maxPlacements), Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt(fmt.Sprintf("github.com/acme/r%d", maxPlacements)), PIDs: []int{11}}
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
	Write(dir, "s", Claim{ProjectID: "p1", Account: testAccount, Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, now)
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
	if Write(dir, "resume", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 10), t0.Add(15*time.Minute)) {
		unlock()
		t.Fatal("unlocked non-Mixed Kind write proceeded while lock was held")
	}
	if Write(dir, "resume", Claim{ProjectID: "p1", Account: testAccount, Repository: repoAt("github.com/acme/web"), PIDs: []int{10}}, t0.Add(16*time.Minute)) {
		unlock()
		t.Fatal("unlocked legacy write proceeded while lock was held")
	}
	unlock()
	savedWait := lockWait
	lockWait = 5 * time.Second
	t.Cleanup(func() { lockWait = savedWait })

	// Untimestamped record spanning a Mark (no kind) and a kinded placement is refused.
	if !Mark(dir, "mixed-mark", "codex", []int{10}, t0) {
		t.Fatal("Mark was not written")
	}
	if !Write(dir, "mixed-mark", placed(KindOutsideGit, "", "/tmp/a", "r1", 10), t0.Add(time.Minute)) {
		t.Fatal("kinded placement after Mark was not written")
	}
	if got, ok := mustRead(t, dir, "mixed-mark", t0.Add(time.Minute)).At(10, time.Time{}); ok {
		t.Fatalf("untimed record spanning Mark and KindOutsideGit was covered: %+v", got)
	}

	// Out-of-order delayed Mark at t1 between collected writes at t0 and t2 keeps its boundary at t1.
	if !Write(dir, "delayed-mark", Claim{ProjectID: "p1", Account: testAccount, Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, t0) {
		t.Fatal("delayed-mark t0 write failed")
	}
	if !Write(dir, "delayed-mark", Claim{ProjectID: "p2", Account: testAccount, Repository: repoAt("github.com/acme/web"), PIDs: []int{10}}, t0.Add(2*time.Minute)) {
		t.Fatal("delayed-mark t2 write failed")
	}
	if !Mark(dir, "delayed-mark", "codex", []int{10}, t0.Add(time.Minute)) {
		t.Fatal("delayed-mark t1 Mark failed")
	}
	if got, ok := mustRead(t, dir, "delayed-mark", t0.Add(3*time.Minute)).At(10, t0.Add(90*time.Second)); !ok || got.ProjectID != "" {
		t.Fatalf("delayed Mark at t1+30s = %+v, %v; want exclusion Mark", got, ok)
	}
}

// A locked KindMixed write from one process in a run must cover every process already
// placed in that run (and same account/profile lane) from the crossing onward, while
// leaving pre-crossing records intact and never leaking PIDs across accounts.
func TestLockedMixedPoisonsAllProcessesInSameRun(t *testing.T) {
	dir := enable(t)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if !Write(dir, "multi-pid", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0) {
		t.Fatal("r1 PID 10 write failed")
	}
	if !Write(dir, "multi-pid", placed(KindOutsideGit, "", "/tmp/scratch", "r1", 12), t0.Add(time.Minute)) {
		t.Fatal("r1 PID 12 write failed")
	}
	if !Write(dir, "multi-pid", placed(KindMixed, "", "", "r1", 11), t0.Add(2*time.Minute)) {
		t.Fatal("r1 PID 11 locked Mixed write failed")
	}
	c := mustRead(t, dir, "multi-pid", t0.Add(3*time.Minute))
	if got, ok := c.At(10, t0.Add(30*time.Second)); !ok || got.Kind != KindRepository {
		t.Fatalf("PID 10 before crossing = %+v, %v; want KindRepository", got, ok)
	}
	for _, pid := range []int{10, 11, 12} {
		if got, ok := c.At(pid, t0.Add(2*time.Minute)); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("PID %d at crossing = %+v, %v; want KindMixed", pid, got, ok)
		}
	}
	if got, ok := c.At(10, time.Time{}); ok {
		t.Fatalf("untimed record for PID 10 spanning Repository and Mixed was covered: %+v", got)
	}

	// Delayed locked KindMixed at t1 (t0 < t1 < t2) after Repository@t0 and OutsideGit@t2
	// must cover (t1, t2) as KindMixed without leaving an unpoisoned Repository gap.
	if !Write(dir, "delayed-locked", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0) {
		t.Fatal("delayed-locked repo write failed")
	}
	if !Write(dir, "delayed-locked", placed(KindOutsideGit, "", "/tmp/scratch", "r1", 10), t0.Add(2*time.Minute)) {
		t.Fatal("delayed-locked outside-git write failed")
	}
	if !Write(dir, "delayed-locked", placed(KindMixed, "", "", "r1", 10), t0.Add(time.Minute)) {
		t.Fatal("delayed-locked Mixed write failed")
	}
	cDelayed := mustRead(t, dir, "delayed-locked", t0.Add(3*time.Minute))
	if got, ok := cDelayed.At(10, t0.Add(30*time.Second)); !ok || got.Kind != KindRepository {
		t.Fatalf("delayed-locked before t1 = %+v, %v; want KindRepository", got, ok)
	}
	for _, at := range []time.Time{t0.Add(90 * time.Second), t0.Add(3 * time.Minute)} {
		if got, ok := cDelayed.At(10, at); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("delayed-locked At(%v) = %+v, %v; want KindMixed", at, got, ok)
		}
	}
	// An even earlier locked KindMixed at t0+20s lowers the boundary in place.
	if !Write(dir, "delayed-locked", placed(KindMixed, "", "", "r1", 10), t0.Add(20*time.Second)) {
		t.Fatal("earlier locked Mixed write failed")
	}
	if got, ok := mustRead(t, dir, "delayed-locked", t0.Add(3*time.Minute)).At(10, t0.Add(30*time.Second)); !ok || got.Kind != KindMixed || got.ProjectID != "" {
		t.Fatalf("delayed-locked after lowering to t0+20s = %+v, %v; want KindMixed", got, ok)
	}

	// Cross-account locked KindMixed must not inherit another account's PIDs, and an
	// interleaved placement from another account must not push Bob's delayed crossing past t1.
	alice := placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10)
	alice.Account, alice.Profile = "aaaaaaaaaaaaaaaa", "work"
	if !Write(dir, "cross-acct-locked", alice, t0) {
		t.Fatal("alice write failed")
	}
	bob := placed(KindMixed, "", "", "r1", 20)
	bob.Account, bob.Profile = "bbbbbbbbbbbbbbbb", "work"
	if !Write(dir, "cross-acct-locked", bob, t0.Add(time.Minute)) {
		t.Fatal("bob locked Mixed write failed")
	}
	got := mustRead(t, dir, "cross-acct-locked", t0.Add(2*time.Minute))
	if got.Kind != KindMixed || got.Account != "bbbbbbbbbbbbbbbb" || slices.Contains(got.PIDs, 10) {
		t.Fatalf("cross-account locked Mixed leaked PIDs: %+v", got)
	}

	bobRepo := placed(KindRepository, "github.com/acme/web", "/src/web", "r1", 20)
	bobRepo.Account, bobRepo.Profile = "bbbbbbbbbbbbbbbb", "work"
	if !Write(dir, "interleaved-lanes", bobRepo, t0) {
		t.Fatal("bob repo write failed")
	}
	if !Write(dir, "interleaved-lanes", alice, t0.Add(2*time.Minute)) {
		t.Fatal("alice repo write failed")
	}
	if !Write(dir, "interleaved-lanes", bob, t0.Add(time.Minute)) {
		t.Fatal("bob delayed Mixed write failed")
	}
	cInter := mustRead(t, dir, "interleaved-lanes", t0.Add(3*time.Minute))
	if got, ok := cInter.At(20, t0.Add(30*time.Second)); !ok || got.Kind != KindRepository {
		t.Fatalf("bob before t1 = %+v, %v; want KindRepository", got, ok)
	}
	if got, ok := cInter.At(20, t0.Add(90*time.Second)); !ok || got.Kind != KindMixed || got.ProjectID != "" {
		t.Fatalf("bob in (t1, t2) = %+v, %v; want KindMixed", got, ok)
	}

	// PID union overflow (> maxPIDs) during Mixed fold fails closed (nil PIDs) rather than
	// dropping an active process back to a collected pre-crossing placement.
	many := make([]int, maxPIDs)
	for i := range many {
		many[i] = 100 + i
	}
	first := placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 65)
	first.PIDs = append([]int{65}, many[:maxPIDs-1]...)
	if !Write(dir, "pid-overflow", first, t0) {
		t.Fatal("pid-overflow initial write failed")
	}
	second := placed(KindOutsideGit, "", "/tmp/work", "r1", 65)
	second.PIDs = []int{65, many[maxPIDs-1]}
	if !Write(dir, "pid-overflow", second, t0.Add(2*time.Minute)) {
		t.Fatal("pid-overflow second write failed")
	}
	pOver, _ := path(dir, "pid-overflow")
	if !writeMixedAside(pOver, placed(KindMixed, "", "", "r1", 99), t0.Add(time.Minute)) {
		t.Fatal("pid-overflow sidecar failed")
	}
	cOver := mustRead(t, dir, "pid-overflow", t0.Add(3*time.Minute))
	if len(cOver.PIDs) != 0 {
		t.Fatalf("pid-overflow did not fail closed to nil PIDs: len=%d", len(cOver.PIDs))
	}
	for _, pid := range []int{65, 99, many[0], many[maxPIDs-1]} {
		if got, ok := cOver.At(pid, t0.Add(3*time.Minute)); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("pid-overflow At(%d) = %+v, %v; want KindMixed", pid, got, ok)
		}
	}
}

// A KindMixed write whose lock timed out lands in the lock-free .mixed sidecar, survives a
// slow non-Mixed lock holder overwriting the main claim file with stale bytes, folds into
// the main file on the next locked write (which still refuses to clear Mixed), and expires
// on Prune.
func TestMixedAsideSurvivesStaleLockHolderAndPrunes(t *testing.T) {
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
	dir := enable(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-TTL - time.Hour)

	t.Run("expired_aside_does_not_resurrect", func(t *testing.T) {
		pExp, _ := path(dir, "expired-aside")
		if !writeMixedAside(pExp, placed(KindMixed, "", "", "r1", 10), old) {
			t.Fatal("writeMixedAside failed")
		}
		entries, err := os.ReadDir(asidePath(pExp))
		if err != nil || len(entries) != 1 {
			t.Fatalf("aside entries = %v, err = %v", entries, err)
		}
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
	})

	t.Run("older_run_aside_keeps_newer_run", func(t *testing.T) {
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
	})

	t.Run("cross_account_aside_and_dedupe", func(t *testing.T) {
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
	})

	t.Run("concurrent_read_during_fold", func(t *testing.T) {
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
						runtime.Gosched()
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
		// On Windows, a concurrent reader's open file handle can block os.Remove during the
		// fold above; once readers exit, a follow-up fold cleans up the remaining mark file.
		if runtime.GOOS == "windows" {
			if entries, err := os.ReadDir(asidePath(pConc)); err == nil && len(entries) > 0 {
				Write(dir, "conc", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(3*time.Second))
			}
		}
		if entries, err := os.ReadDir(asidePath(pConc)); !os.IsNotExist(err) && (runtime.GOOS != "windows" || err != nil || len(entries) > 0) {
			t.Fatalf("conc sidecar dir not cleaned after fold: entries=%d err=%v", len(entries), err)
		}
	})

	t.Run("locked_earlier_run_idempotent_and_clamped", func(t *testing.T) {
		if !Write(dir, "locked-older-run", placed(KindRepository, "github.com/acme/core", "/src/core", "r0", 5), now.Add(-20*time.Minute)) {
			t.Fatal("locked r0 write failed")
		}
		if !Write(dir, "locked-older-run", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(-10*time.Minute)) {
			t.Fatal("locked r1 write failed")
		}
		if !Write(dir, "locked-older-run", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 20), now.Add(-2*time.Minute)) {
			t.Fatal("locked r2 write failed")
		}
		if !Write(dir, "locked-older-run", placed(KindMixed, "", "", "r1", 10), now.Add(-5*time.Minute)) {
			t.Fatal("locked delayed r1 KindMixed write failed")
		}
		for i := range 10 {
			if Write(dir, "locked-older-run", placed(KindMixed, "", "", "r1", 10), now.Add(time.Duration(i)*time.Second)) {
				t.Fatalf("repeat %d of locked earlier-run KindMixed unexpectedly rewrote claim", i)
			}
		}
		if !Write(dir, "locked-older-run", placed(KindMixed, "", "", "r1", 10), now.Add(-7*time.Minute)) {
			t.Fatal("out-of-order earlier r1 KindMixed write failed")
		}
		cLockedOld := mustRead(t, dir, "locked-older-run", now)
		if cLockedOld.Evicted || len(cLockedOld.Placements) != 4 {
			t.Fatalf("repeated/out-of-order earlier-run Mixed grew/evicted placements: evicted=%v len=%d", cLockedOld.Evicted, len(cLockedOld.Placements))
		}
		if got, ok := cLockedOld.At(10, now.Add(-6*time.Minute)); !ok || got.Kind != KindMixed || got.Run != "r1" {
			t.Fatalf("r1 at lowered Since = %+v, %v; want KindMixed", got, ok)
		}
		if cLockedOld.Kind != KindRepository || cLockedOld.Run != "r2" {
			t.Fatalf("latest run after locked earlier Mixed = %+v, want KindRepository for r2", cLockedOld)
		}
		if got, ok := cLockedOld.At(5, now.Add(-19*time.Minute)); !ok || got.Kind != KindRepository || got.Run != "r0" {
			t.Fatalf("r0 after repeated earlier Mixed = %+v, %v; want KindRepository for r0", got, ok)
		}
		if got, ok := cLockedOld.At(10, now.Add(-4*time.Minute)); !ok || got.Kind != KindMixed || got.Run != "r1" {
			t.Fatalf("r1 after locked earlier Mixed = %+v, %v; want KindMixed", got, ok)
		}
		if got, ok := cLockedOld.At(20, now); !ok || got.Kind != KindRepository || got.Run != "r2" {
			t.Fatalf("r2 after locked earlier Mixed = %+v, %v; want KindRepository", got, ok)
		}
		// Post-r2 timestamp on an earlier run's first Mixed write clamps Since to r2's start.
		if !Write(dir, "clamped-older-run", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now.Add(-10*time.Minute)) ||
			!Write(dir, "clamped-older-run", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 20), now.Add(-2*time.Minute)) ||
			!Write(dir, "clamped-older-run", placed(KindMixed, "", "", "r1", 10), now) {
			t.Fatal("clamped-older-run setup failed")
		}
		cClamped := mustRead(t, dir, "clamped-older-run", now)
		if got, ok := cClamped.At(10, now.Add(-time.Minute)); !ok || got.Kind != KindMixed || got.Run != "r1" {
			t.Fatalf("clamped r1 = %+v, %v; want KindMixed", got, ok)
		}
		if got, ok := cClamped.At(20, now); !ok || got.Kind != KindRepository || got.Run != "r2" {
			t.Fatalf("clamped r2 = %+v, %v; want KindRepository", got, ok)
		}
	})

	t.Run("out_of_order_since_and_corrupt_aside", func(t *testing.T) {
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

		// Corrupt unexpired sidecar over an existing non-Mixed claim fails closed for existing PIDs.
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
		if Write(dir, "corrupt-existing", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
			t.Fatal("corrupt sidecar did not block downgrade")
		}
		if got, ok := mustRead(t, dir, "corrupt-existing", now).At(10, now); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("At after corrupt sidecar fold = %+v, %v; want KindMixed", got, ok)
		}
	})

	t.Run("expired_main_early_poison_and_unseen_run", func(t *testing.T) {
		if !Write(dir, "exp-main", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), old) {
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
	})

	t.Run("expired_mixed_main_renewed_by_duplicate_aside", func(t *testing.T) {
		if !Write(dir, "exp-mixed", placed(KindMixed, "", "", "r1", 10), old) {
			t.Fatal("exp-mixed initial write failed")
		}
		pExpMixed, _ := path(dir, "exp-mixed")
		if err := os.Chtimes(pExpMixed, old, old); err != nil {
			t.Fatal(err)
		}
		if !writeMixedAside(pExpMixed, placed(KindMixed, "", "", "r1", 10), now) {
			t.Fatal("exp-mixed duplicate sidecar write failed")
		}
		if Write(dir, "exp-mixed", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
			t.Fatal("non-Mixed write downgraded renewed Mixed claim")
		}
		if got := mustRead(t, dir, "exp-mixed", now); got.Kind != KindMixed || got.Run != "r1" {
			t.Fatalf("Read after duplicate sidecar fold = %+v, want KindMixed r1", got)
		}
		Prune(dir, now)
		if got := mustRead(t, dir, "exp-mixed", now); got.Kind != KindMixed || got.Run != "r1" {
			t.Fatalf("Read after Prune = %+v, want KindMixed r1", got)
		}
		if Write(dir, "exp-mixed", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), now) {
			t.Fatal("same-run non-Mixed write replaced renewed Mixed claim after Prune")
		}
	})

	t.Run("unreadable_aside_dir_fails_closed", func(t *testing.T) {
		if !Write(dir, "bad-aside", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, now) {
			t.Fatal("initial claim write failed")
		}
		pBad, _ := path(dir, "bad-aside")
		// A regular file at the .mixed path makes os.ReadDir fail with a non-ErrNotExist error on all OSes.
		if err := os.WriteFile(asidePath(pBad), []byte("not-a-dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := mustRead(t, dir, "bad-aside", now)
		if got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("Read with unreadable .mixed sidecar = %+v, want KindMixed fail-closed", got)
		}
		if Write(dir, "bad-aside", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, now) {
			t.Fatal("non-Mixed write succeeded despite unreadable .mixed sidecar")
		}
	})

	t.Run("delayed_non_mixed_earlier_run_blocked", func(t *testing.T) {
		t0 := now.Add(-10 * time.Minute)
		if !Write(dir, "early-block", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0) {
			t.Fatal("r1 repo write failed")
		}
		if !Write(dir, "early-block", placed(KindRepository, "github.com/acme/web", "/src/web", "r2", 20), t0.Add(4*time.Minute)) {
			t.Fatal("r2 repo write failed")
		}
		if !Write(dir, "early-block", placed(KindMixed, "", "", "r1", 10), t0.Add(2*time.Minute)) {
			t.Fatal("r1 locked Mixed fold failed")
		}
		if Write(dir, "early-block", placed(KindRepository, "github.com/acme/api", "/src/api", "r1", 10), t0.Add(3*time.Minute)) {
			t.Fatal("delayed non-Mixed write downgraded earlier Mixed run r1 behind r2")
		}
		c := mustRead(t, dir, "early-block", now)
		if got, ok := c.At(10, t0.Add(3*time.Minute)); !ok || got.Kind != KindMixed {
			t.Fatalf("r1 after crossing = %+v, %v; want KindMixed", got, ok)
		}
		if got, ok := c.At(20, now); !ok || got.Kind != KindRepository || got.Run != "r2" {
			t.Fatalf("r2 = %+v, %v; want KindRepository r2", got, ok)
		}
	})

	t.Run("nil_pids_widens_finite_mixed", func(t *testing.T) {
		// Locked path: finite-PID Mixed claim must widen to nil (all processes) within Refresh.
		if !Write(dir, "widen-lock", placed(KindMixed, "", "", "r1", 10), now) {
			t.Fatal("initial finite Mixed write failed")
		}
		if !Write(dir, "widen-lock", placed(KindMixed, "", "", "r1"), now.Add(time.Second)) {
			t.Fatal("nil-PID Mixed write was skipped instead of widening finite Mixed claim")
		}
		if got := mustRead(t, dir, "widen-lock", now.Add(time.Second)); len(got.PIDs) != 0 || !got.Covers(999) {
			t.Fatalf("widened locked Mixed = %+v, want nil PIDs covering all processes", got)
		}

		// Sidecar path: nil-PID sidecar mark must not dedupe against an existing finite-PID sidecar mark.
		pW, _ := path(dir, "widen-aside")
		if !writeMixedAside(pW, placed(KindMixed, "", "", "r1", 10), now) {
			t.Fatal("initial finite aside failed")
		}
		if !writeMixedAside(pW, placed(KindMixed, "", "", "r1"), now.Add(time.Second)) {
			t.Fatal("nil-PID aside failed")
		}
		if got := mustRead(t, dir, "widen-aside", now.Add(time.Second)); len(got.PIDs) != 0 || !got.Covers(999) {
			t.Fatalf("widened aside Mixed = %+v, want nil PIDs covering all processes", got)
		}
	})

	t.Run("evicted_mixed_tombstone_blocks_old_run", func(t *testing.T) {
		t0 := now.Add(-time.Hour)
		if !Write(dir, "evict-mix", placed(KindMixed, "", "", "r0", 10), t0) {
			t.Fatal("initial Mixed write failed")
		}
		for i := 1; i <= maxPlacements; i++ {
			run := fmt.Sprintf("r%d", i)
			repo := fmt.Sprintf("github.com/acme/repo-%d", i)
			if !Write(dir, "evict-mix", placed(KindRepository, repo, "/src/"+run, run, 10+i), t0.Add(time.Duration(i)*time.Minute)) {
				t.Fatalf("write for %s failed", run)
			}
		}
		c := mustRead(t, dir, "evict-mix", now)
		if !c.Evicted || !slices.Contains(c.MixedRuns, "r0") {
			t.Fatalf("expected evicted tombstone for r0, got evicted=%v mixed_runs=%v", c.Evicted, c.MixedRuns)
		}
		// Delayed non-Mixed write for evicted Mixed run r0 is refused.
		if Write(dir, "evict-mix", placed(KindRepository, "github.com/acme/api", "/src/api", "r0", 10), now) {
			t.Fatal("delayed non-Mixed write resurrected evicted Mixed run r0")
		}
		// Delayed KindMixed write for evicted r0 is a no-op and does not poison the active tail run.
		if Write(dir, "evict-mix", placed(KindMixed, "", "", "r0", 10), now) {
			t.Fatal("delayed Mixed write for evicted r0 unexpectedly rewrote active tail")
		}
		// A brand-new clean run still succeeds.
		if !Write(dir, "evict-mix", placed(KindRepository, "github.com/acme/fresh", "/src/fresh", "r-new", 99), now.Add(time.Second)) {
			t.Fatal("new clean run after Mixed eviction was blocked")
		}
		if got := mustRead(t, dir, "evict-mix", now.Add(time.Second)); got.Kind != KindRepository || got.Run != "r-new" {
			t.Fatalf("after new clean run = %+v, want KindRepository r-new", got)
		}
	})

	t.Run("tombstone_overflow_applies_new_mixed_marks", func(t *testing.T) {
		t0 := now.Add(-2 * time.Hour)
		// Evict maxPIDs+1 distinct Mixed runs so MixedRuns overflows to ["*"].
		for i := range maxPlacements + maxPIDs + 1 {
			run := fmt.Sprintf("m%d", i)
			if !Write(dir, "evict-over", placed(KindMixed, "", "", run, 10), t0.Add(time.Duration(i)*time.Second)) {
				t.Fatalf("mixed write %s failed", run)
			}
		}
		if got := mustRead(t, dir, "evict-over", now); !slices.Equal(got.MixedRuns, []string{"*"}) {
			t.Fatalf("evict-over MixedRuns = %v, want [*]", got.MixedRuns)
		}
		// Seed a collected clean tail with MixedRuns == ["*"] and verify a new Mixed run is still applied.
		for _, tc := range []struct {
			name  string
			aside bool
		}{
			{name: "locked", aside: false},
			{name: "aside", aside: true},
		} {
			sid := "over-" + tc.name
			if !Write(dir, sid, placed(KindRepository, "github.com/acme/api", "/src/api", "r-clean", 10), t0) {
				t.Fatalf("%s initial clean write failed", tc.name)
			}
			p, _ := path(dir, sid)
			prev, ok := read(p)
			if !ok {
				t.Fatalf("%s read failed", tc.name)
			}
			prev.Evicted, prev.MixedRuns = true, []string{"*"}
			if !persist(p, prev, nil) {
				t.Fatalf("%s persist wildcard tombstone failed", tc.name)
			}
			// A new Mixed run sharing PID 10 must NOT be discarded by MixedRuns == ["*"].
			newMix := placed(KindMixed, "", "", "r-new-mix", 10)
			if tc.aside {
				if !writeMixedAside(p, newMix, now) {
					t.Fatalf("%s sidecar write failed", tc.name)
				}
			} else if !Write(dir, sid, newMix, now) {
				t.Fatalf("%s locked new Mixed write was discarded under wildcard tombstone", tc.name)
			}
			c := mustRead(t, dir, sid, now)
			if got, ok := c.At(10, now); !ok || got.Kind != KindMixed || got.Run != "r-new-mix" {
				t.Fatalf("%s At(10, now) = %+v, %v; want KindMixed r-new-mix", tc.name, got, ok)
			}
		}
	})

	t.Run("mark_lands_after_runless_and_wildcard_eviction", func(t *testing.T) {
		t0 := now.Add(-2 * time.Hour)
		// Runless Mixed placement evicted by maxPlacements named collected runs leaves "" in MixedRuns.
		if !Write(dir, "mark-runless", placed(KindMixed, "", "", "", 10), t0) {
			t.Fatal("initial runless Mixed write failed")
		}
		for i := 1; i <= maxPlacements; i++ {
			run := fmt.Sprintf("c%d", i)
			c := Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: run, Repository: repoAt(fmt.Sprintf("github.com/acme/r%d", i)), PIDs: []int{10}}
			if !Write(dir, "mark-runless", c, t0.Add(time.Duration(i)*time.Minute)) {
				t.Fatalf("collected write %s failed", run)
			}
		}
		if !Mark(dir, "mark-runless", "codex", []int{10}, now) {
			t.Fatal("Mark was rejected after runless Mixed eviction")
		}
		if got, ok := mustRead(t, dir, "mark-runless", now).At(10, now); !ok || got.ProjectID != "" {
			t.Fatalf("after Mark on runless-evicted tail = %+v, %v; want no project", got, ok)
		}

		// Wildcard overflow ("*") on a collected tail must also allow exclusion Mark.
		if !Write(dir, "mark-wild", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, t0) {
			t.Fatal("mark-wild initial write failed")
		}
		pW, _ := path(dir, "mark-wild")
		prev, _ := read(pW)
		prev.Evicted, prev.MixedRuns = true, []string{"*"}
		if !persist(pW, prev, nil) {
			t.Fatal("mark-wild persist failed")
		}
		if !Mark(dir, "mark-wild", "codex", []int{10}, now) {
			t.Fatal("Mark was rejected under wildcard MixedRuns overflow")
		}
		if got, ok := mustRead(t, dir, "mark-wild", now).At(10, now); !ok || got.ProjectID != "" {
			t.Fatalf("after Mark on wildcard-evicted tail = %+v, %v; want no project", got, ok)
		}
	})

	t.Run("wildcard_overflow_does_not_leave_stale_collected_tail", func(t *testing.T) {
		t0 := now.Add(-2 * time.Hour)
		// Fill maxPlacements + maxPIDs distinct Mixed runs, then one collected placement on PID 10
		// so MixedRuns has maxPIDs entries (one shy of overflow) while PID 10 is collected.
		for i := range maxPlacements + maxPIDs - 1 {
			run := fmt.Sprintf("w%d", i)
			if !Write(dir, "wild-tail", placed(KindMixed, "", "", run, 10), t0.Add(time.Duration(i)*time.Second)) {
				t.Fatalf("mixed write %s failed", run)
			}
		}
		if !Write(dir, "wild-tail", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r-over", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, t0.Add(30*time.Minute)) {
			t.Fatal("pre-overflow collected write failed")
		}
		// Another process (PID 11) in the same run switches repos, triggering the final Mixed eviction -> ["*"].
		overflowAt := t0.Add(time.Hour)
		if !Write(dir, "wild-tail", Claim{ProjectID: "p2", Account: testAccount, Kind: KindRepository, Run: "r-over", Repository: repoAt("github.com/acme/web"), PIDs: []int{11}}, overflowAt) {
			t.Fatal("overflow write failed")
		}
		cOver := mustRead(t, dir, "wild-tail", now)
		for _, pid := range []int{10, 11} {
			if got, ok := cOver.At(pid, overflowAt); !ok || got.Kind != KindMixed || got.ProjectID != "" {
				t.Fatalf("overflowing write At(pid=%d) = %+v, %v; want KindMixed with empty ProjectID", pid, got, ok)
			}
		}
		// Even if a stored claim already has MixedRuns=["*"] beside a collected tail, a new clean run
		// must fail closed rather than leaving the old collected project active for later records.
		if !Write(dir, "wild-stale", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, t0) {
			t.Fatal("wild-stale initial write failed")
		}
		pS, _ := path(dir, "wild-stale")
		prev, _ := read(pS)
		prev.Evicted, prev.MixedRuns = true, []string{"*"}
		if !persist(pS, prev, nil) {
			t.Fatal("wild-stale persist failed")
		}
		if !Write(dir, "wild-stale", Claim{ProjectID: "p2", Account: testAccount, Kind: KindRepository, Run: "r2", Repository: repoAt("github.com/acme/web"), PIDs: []int{11}}, now) {
			t.Fatal("clean write after wildcard overflow failed to persist fail-closed boundary")
		}
		if got, ok := mustRead(t, dir, "wild-stale", now).At(11, now); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("after clean run on wildcard tail = %+v, %v; want KindMixed with empty ProjectID", got, ok)
		}

		// Earlier retained run r1 sharing PID 10 with tail r2 under MixedRuns=["*"] must still append a tail Mixed boundary.
		if !Write(dir, "wild-earlier", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, t0) ||
			!Write(dir, "wild-earlier", Claim{ProjectID: "p2", Account: testAccount, Kind: KindRepository, Run: "r2", Repository: repoAt("github.com/acme/web"), PIDs: []int{10}}, t0.Add(time.Minute)) {
			t.Fatal("wild-earlier setup failed")
		}
		pE, _ := path(dir, "wild-earlier")
		prevE, _ := read(pE)
		prevE.Evicted, prevE.MixedRuns = true, []string{"*"}
		if !persist(pE, prevE, nil) {
			t.Fatal("wild-earlier persist failed")
		}
		if !Write(dir, "wild-earlier", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/api"), PIDs: []int{10}}, now) {
			t.Fatal("earlier retained run write under wildcard overflow did not persist Mixed tail")
		}
		if got, ok := mustRead(t, dir, "wild-earlier", now).At(10, now); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("after earlier retained run under wildcard overflow = %+v, %v; want KindMixed", got, ok)
		}
	})

	t.Run("runless_eviction_boundary_and_refresh_fast_path", func(t *testing.T) {
		t0 := now.Add(-2 * time.Hour)
		// Boundary: runless Mixed + (maxPlacements-1) named placements; a runless collected write
		// is rejected while the retained Mixed placement remains, so PID 11 is never routed and t0 stays Mixed.
		if !Write(dir, "runless-bound", placed(KindMixed, "", "", "", 10), t0) {
			t.Fatal("initial runless Mixed write failed")
		}
		for i := 1; i < maxPlacements; i++ {
			run := fmt.Sprintf("b%d", i)
			if !Write(dir, "runless-bound", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: run, Repository: repoAt(fmt.Sprintf("github.com/acme/b%d", i)), PIDs: []int{10}}, t0.Add(time.Duration(i)*time.Minute)) {
				t.Fatalf("named write %s failed", run)
			}
		}
		evictAt := t0.Add(time.Hour)
		if Write(dir, "runless-bound", Claim{ProjectID: "p2", Account: testAccount, Kind: KindRepository, Repository: repoAt("github.com/acme/evict"), PIDs: []int{11}}, evictAt) {
			t.Fatal("runless write succeeded while a retained Mixed placement remained")
		}
		cBound := mustRead(t, dir, "runless-bound", now)
		if got, ok := cBound.At(11, evictAt); ok && got.ProjectID != "" {
			t.Fatalf("boundary At(pid=11) = %+v, %v; want unrouted", got, ok)
		}
		if got, ok := cBound.At(10, t0); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("boundary At(pid=10, t0) = %+v, %v; want KindMixed", got, ok)
		}

		// Fast path: after runless Mixed is evicted by maxPlacements named writes, a matching runless
		// collected write within Refresh must NOT take canSkipRefresh and leave the collected tail active.
		if !Write(dir, "runless-fast", placed(KindMixed, "", "", "", 10), t0) {
			t.Fatal("runless-fast initial write failed")
		}
		var lastRepo config.Repository
		for i := 1; i <= maxPlacements; i++ {
			run := fmt.Sprintf("f%d", i)
			lastRepo = repoAt(fmt.Sprintf("github.com/acme/f%d", i))
			if !Write(dir, "runless-fast", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: run, Repository: lastRepo, PIDs: []int{10}}, now.Add(-time.Duration(maxPlacements-i+1)*time.Second)) {
				t.Fatalf("runless-fast named write %s failed", run)
			}
		}
		if !Write(dir, "runless-fast", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Repository: lastRepo, PIDs: []int{10}}, now) {
			t.Fatal("runless write within Refresh was skipped by fast path instead of failing closed")
		}
		if got, ok := mustRead(t, dir, "runless-fast", now).At(10, now); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("after runless refresh write = %+v, %v; want KindMixed", got, ok)
		}

		// After a named Mixed run is evicted, a runless collected write cannot prove it is a new run and must fail closed.
		if !Write(dir, "named-then-runless", placed(KindMixed, "r0", "", "", 10), t0) {
			t.Fatal("initial named Mixed write failed")
		}
		for i := 1; i <= maxPlacements; i++ {
			run := fmt.Sprintf("n%d", i)
			if !Write(dir, "named-then-runless", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: run, Repository: repoAt(fmt.Sprintf("github.com/acme/n%d", i)), PIDs: []int{10}}, t0.Add(time.Duration(i)*time.Minute)) {
				t.Fatalf("named write %s failed", run)
			}
		}
		if !Write(dir, "named-then-runless", Claim{ProjectID: "p2", Account: testAccount, Kind: KindRepository, Repository: repoAt("github.com/acme/late"), PIDs: []int{10}}, now) {
			t.Fatal("runless write after named Mixed eviction failed")
		}
		if got, ok := mustRead(t, dir, "named-then-runless", now).At(10, now); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("runless write after named Mixed eviction = %+v, %v; want KindMixed", got, ok)
		}

		// While a retained named Mixed placement remains, a delayed runless collected write must not downgrade its PID.
		if !Write(dir, "retained-mixed-runless", placed(KindMixed, "r0", "", "", 10), t0) ||
			!Write(dir, "retained-mixed-runless", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/r1"), PIDs: []int{20}}, t0.Add(time.Minute)) {
			t.Fatal("retained-mixed-runless setup failed")
		}
		Write(dir, "retained-mixed-runless", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Repository: repoAt("github.com/acme/r1"), PIDs: []int{10}}, now)
		cRet := mustRead(t, dir, "retained-mixed-runless", now)
		if got, ok := cRet.At(10, now); !ok || got.Kind != KindMixed || got.ProjectID != "" {
			t.Fatalf("PID 10 after delayed runless write = %+v, %v; want KindMixed", got, ok)
		}
		if got, ok := cRet.At(20, now); !ok || got.Kind != KindRepository || got.ProjectID != "p1" {
			t.Fatalf("PID 20 on clean r1 = %+v, %v; want KindRepository p1", got, ok)
		}

		// Earlier-run parent-bearing KindMixed sidecar folded onto a newer run must preserve Parent.
		if !Write(dir, "aside-earlier-parent", Claim{ProjectID: "p1", Account: testAccount, Kind: KindRepository, Run: "r0", Repository: repoAt("github.com/acme/r0"), PIDs: []int{10}}, t0) ||
			!Write(dir, "aside-earlier-parent", Claim{ProjectID: "p2", Account: testAccount, Kind: KindRepository, Run: "r1", Repository: repoAt("github.com/acme/r1"), PIDs: []int{20}}, t0.Add(time.Minute)) {
			t.Fatal("aside-earlier-parent setup failed")
		}
		pAside, _ := path(dir, "aside-earlier-parent")
		if !writeMixedAside(pAside, Claim{Kind: KindMixed, Run: "r0", Account: testAccount, Parent: "parent-sess", PIDs: []int{10}}, t0.Add(30*time.Second)) {
			t.Fatal("writeMixedAside failed")
		}
		cAside := mustRead(t, dir, "aside-earlier-parent", now)
		if cAside.Parent != "parent-sess" {
			t.Fatalf("Parent after earlier-run sidecar fold = %q, want parent-sess", cAside.Parent)
		}
		if got, ok := cAside.At(10, t0.Add(45*time.Second)); !ok || got.Kind != KindMixed || got.Parent != "parent-sess" {
			t.Fatalf("At(10) after earlier-run sidecar fold = %+v, %v; want KindMixed with parent-sess", got, ok)
		}
	})
}
