package claim

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func repoAt(origin string) config.Repository { return config.Repository{Origin: origin} }

// placed is a location-only placement: a kind and where, no project.
func placed(kind Kind, origin, cwd string, pids ...int) Claim {
	c := Claim{Kind: kind, Cwd: cwd, PIDs: pids}
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
	if !Write(dir, "s", placed(KindOutsideGit, "", "/home/dev/scratch", 10), now) {
		t.Fatal("a location-only placement was refused")
	}
	c := mustRead(t, dir, "s", now)
	if c.Kind != KindOutsideGit || c.ProjectID != "" || len(c.Placements) != 1 || c.Placements[0].Kind != KindOutsideGit {
		t.Fatalf("claim %+v", c)
	}
	if Write(dir, "s", placed(KindOutsideGit, "", "/home/dev/scratch", 10), now) {
		t.Fatal("the same location within Refresh rewrote the claim")
	}
	if Write(dir, "s2", Claim{Tool: "codex"}, now) {
		t.Fatal("a placement with neither a project nor a kind was written")
	}
	if Write(dir, "s2", Claim{ProjectID: "p1", Kind: KindRepository}, now) {
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
		{placed(KindRepository, "github.com/acme/api", "/src/api", 10), 1},
		{placed(KindRepository, "github.com/acme/api", "/src/api/web", 10), 1},
		{placed(KindRepository, "github.com/acme/web", "/src/web", 10), 2},
		{placed(KindOutsideGit, "", "/tmp/a", 10), 3},
		{placed(KindOutsideGit, "", "/tmp/a", 10), 3},
		{placed(KindOutsideGit, "", "/tmp/b", 10), 4},
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
	if !Write(dir, "s", placed(KindOutsideGit, "", "/tmp/a", 10), t1) {
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

// writeSpread writes n placements in distinct repositories, a minute apart from t0, for
// pid 10, and returns the claim.
func writeSpread(t *testing.T, dir, sid string, t0 time.Time, n int, kinds bool) Claim {
	t.Helper()
	for i := range n {
		c := Claim{ProjectID: fmt.Sprintf("p%d", i), Account: testAccount, Repository: repoAt(fmt.Sprintf("github.com/acme/r%d", i)), PIDs: []int{10}}
		if kinds {
			c.Kind = KindRepository
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
	latest := Claim{ProjectID: fmt.Sprintf("p%d", maxPlacements), Account: testAccount, Kind: KindRepository, Repository: repoAt(fmt.Sprintf("github.com/acme/r%d", maxPlacements)), PIDs: []int{11}}
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
	Write(dir, "s", placed(KindRepository, "github.com/acme/api", "/src/api", 10), t0)
	Write(dir, "s", placed(KindOutsideGit, "", "/tmp/a", 10), t0.Add(time.Minute))
	c := mustRead(t, dir, "s", t0)
	if got, ok := c.At(10, t0.Add(-time.Second)); !ok || got.Kind != KindRepository {
		t.Fatalf("the racing export = %+v, %v", got, ok)
	}
	// Another run's first records, in its own process, are its own too.
	Write(dir, "s", placed(KindRepository, "github.com/acme/web", "/src/web", 20), t0.Add(2*time.Minute))
	c = mustRead(t, dir, "s", t0)
	if got, ok := c.At(20, t0.Add(30*time.Second)); !ok || got.Repository.Origin != "github.com/acme/web" {
		t.Fatalf("the second run's early record = %+v, %v", got, ok)
	}
}

// A record without a time uses the latest placement only if nothing was evicted and every
// placement covering it has one Kind and Repository.
func TestUntimestampedRecords(t *testing.T) {
	t.Parallel()
	dir := enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	Write(dir, "outside", placed(KindOutsideGit, "", "/tmp/a", 10), t0)
	Write(dir, "outside", placed(KindOutsideGit, "", "/tmp/b", 10), t0.Add(time.Minute))
	if got, ok := mustRead(t, dir, "outside", t0).At(10, time.Time{}); !ok || got.Cwd != "/tmp/b" {
		t.Fatalf("two directories outside git = %+v, %v; want the latest", got, ok)
	}

	Write(dir, "moved", placed(KindRepository, "github.com/acme/api", "/src/api", 10), t0)
	Write(dir, "moved", placed(KindOutsideGit, "", "/tmp/a", 10), t0.Add(time.Minute))
	if got, ok := mustRead(t, dir, "moved", t0).At(10, time.Time{}); ok {
		t.Fatalf("a record without a time across two locations = %+v", got)
	}

	// Two runs in their own processes: a record's process alone places it.
	Write(dir, "runs", placed(KindRepository, "github.com/acme/api", "/src/api", 10), t0)
	Write(dir, "runs", placed(KindRepository, "github.com/acme/web", "/src/web", 20), t0.Add(time.Minute))
	c := mustRead(t, dir, "runs", t0)
	if got, ok := c.At(10, time.Time{}); !ok || got.Repository.Origin != "github.com/acme/api" {
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
	work := placed(KindRepository, "github.com/acme/api", "", 10)
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

	alice := placed(KindRepository, "github.com/acme/api", "/src/api", 10)
	alice.Account, alice.Profile, alice.Parent, alice.CatchAll = "aaaaaaaaaaaaaaaa", "default", "parent-sess", true
	if !Write(dir, "s", alice, t0) {
		t.Fatal("alice placement was not written")
	}

	// Same repo under Bob's account is a distinct placement; blank Parent keeps prev.Parent.
	bob := placed(KindRepository, "github.com/acme/api", "/src/api", 10)
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
