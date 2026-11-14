package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const (
	teamAuth  = "https://auth.example"
	orgAcme   = "org-acme"
	orgMe     = "org-me"
	teamKeyA  = "ter_srv_acmeteam0123456789abcdefghijklmnopq"
	staleKeyA = "ter_srv_acmestale0123456789abcdefghijklmnop"
)

// migratedMachine is a machine the new relay migrated: credentials.v2.json marks it.
func migratedMachine(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(config.CredentialsV2Path(dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dir: dir, StateDir: dir, ProfileName: config.DefaultProfile, AuthURL: teamAuth, OTLPURL: "https://otel.example", APIURL: "https://api.example"}
	if err := config.UpdateProfile(dir, cfg.ProfileName, func(p *config.Profile) { p.Harnesses = []string{"claude"} }); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func teamPolicy(teamID, org string, fetched time.Time, repos ...string) config.Policy {
	p := config.Policy{Mode: config.ModeRepo, Repositories: repos, Wildcards: true, IncludePrompts: true, OrganizationID: org, AuthURL: teamAuth,
		TeamID: teamID, Revision: 1, FetchedAt: fetched}
	if repos == nil {
		p.Mode = config.ModeGlobal
	}
	return p
}

// selectTeams writes cfg's committed routing state with acme (github.com/acme/*) and,
// when personal, the developer's personal team collecting everything; bindings are
// applied after. It returns the enrollments by team.
func selectTeams(t *testing.T, cfg *config.Config, personal bool, change func(*routing.State)) map[string]string {
	t.Helper()
	now := time.Now()
	st, err := routing.Create(cfg.StateDir, cfg.ProfileName, func(s *routing.State) error {
		if _, err := s.Select(routing.Team{OrganizationID: orgAcme, AuthURL: teamAuth, TeamID: "acme", Policy: teamPolicy("acme", orgAcme, now, "github.com/acme/*")}); err != nil {
			return err
		}
		if personal {
			if _, err := s.Select(routing.Team{OrganizationID: orgMe, AuthURL: teamAuth, TeamID: "me", Personal: true, Policy: teamPolicy("me", orgMe, now)}); err != nil {
				return err
			}
		}
		s.Migration = &routing.Migration{Phase: routing.Committed}
		if change != nil {
			change(s)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, tm := range st.Teams {
		ids[tm.TeamID] = tm.EnrollmentID
	}
	return ids
}

func update(t *testing.T, cfg *config.Config, change func(*routing.State)) {
	t.Helper()
	if _, err := routing.Update(cfg.StateDir, cfg.ProfileName, func(s *routing.State) error { change(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func placed(kind claim.Kind, origin string) claim.Claim {
	return claim.Claim{Kind: kind, Repository: config.Repository{Origin: origin}}
}

// Each record goes to the team routing names for where it ran: acme's bound repositories
// to acme, everything else to personal, a Mixed placement nowhere.
func TestTeamsRouteEachRecordByWhereItRan(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, func(s *routing.State) { _ = s.Bind("github.com/acme", orgAcme, false) })
	teams := NewTeams(cfg, TeamsDeps{})
	if !teams.Migrated() {
		t.Fatal("a migrated machine is not taken for one")
	}
	at := time.Now()
	for _, tc := range []struct {
		name    string
		rec     relay.Record
		outcome relay.Outcome
		to      string
		why     relay.Why
	}{
		{"acme's repository", relay.Record{Session: "s1", Placed: true, Placement: placed(claim.KindRepository, "github.com/acme/api"), At: at}, relay.Send, ids["acme"], 0},
		{"a personal repository", relay.Record{Session: "s1", Placed: true, Placement: placed(claim.KindRepository, "github.com/dawson/side"), At: at}, relay.Send, ids["me"], 0},
		{"outside git", relay.Record{Session: "s1", Placed: true, Placement: placed(claim.KindOutsideGit, ""), At: at}, relay.Send, ids["me"], 0},
		{"a legacy placement names its repository", relay.Record{Session: "s1", Placed: true, Placement: claim.Claim{ProjectID: "acme", Repository: config.Repository{Origin: "github.com/acme/api"}}, At: at}, relay.Send, ids["acme"], 0},
		{"mixed", relay.Record{Session: "s1", Placed: true, Placement: placed(claim.KindMixed, ""), At: at}, relay.Drop, "", relay.WhyMixed},
		// Something is bound to an organization, so an unknown record is never personal's.
		{"unknown", relay.Record{Session: "s1", At: at}, relay.Hold, "", relay.WhyUnknown},
	} {
		got := teams.Route(tc.rec)
		if got.Outcome != tc.outcome || got.Queued.Enrollment != tc.to || got.Outcome != relay.Send && got.Why != tc.why {
			t.Errorf("%s: %+v", tc.name, got)
		}
		if got.Outcome == relay.Send && (got.Queued.Session != "s1" || !got.Queued.At.Equal(at)) {
			t.Errorf("%s: queued %+v without its run and time", tc.name, got.Queued)
		}
	}
	if got := teams.Route(relay.Record{Placed: true, Placement: placed(claim.KindRepository, "github.com/acme/api")}); got.Queued.Project != "acme" {
		t.Errorf("acme's record would be stamped %q", got.Queued.Project)
	}
}

// A record whose run was Mixed by its time is dropped before routing and before any
// catch-all, with the Mixed time for its queue; one made before it is not.
func TestTeamsDropAMixedRunFirst(t *testing.T) {
	cfg := migratedMachine(t)
	// Personal alone.
	selectTeams(t, cfg, true, func(s *routing.State) { s.Deselect(s.Teams[0].EnrollmentID) })
	teams := NewTeams(cfg, TeamsDeps{})
	if !teams.UnknownToPersonal() {
		t.Fatal("personal alone, collecting everything, with nothing bound, takes unknown records")
	}
	start := time.Now().Add(-time.Hour)
	if _, err := runs.Start(cfg.StateDir, "s1", start); err != nil {
		t.Fatal(err)
	}
	mixedAt := start.Add(30 * time.Minute)
	if err := runs.MarkMixed(cfg.StateDir, "s1", mixedAt, runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}
	got := teams.Route(relay.Record{Session: "s1", At: mixedAt.Add(time.Minute)})
	if got.Outcome != relay.Drop || got.Why != relay.WhyMixed || !got.MixedAt.Equal(mixedAt) || got.Blocker != "mixed:s1" {
		t.Fatalf("after the crossing: %+v", got)
	}
	if got := teams.Route(relay.Record{Session: "s1", At: mixedAt.Add(-time.Minute)}); got.Outcome != relay.Send {
		t.Fatalf("before the crossing: %+v", got)
	}
	if got := teams.Route(relay.Record{Session: "s1", At: mixedAt.Add(time.Minute), Narrow: true}); got.Outcome != relay.Drop {
		t.Fatalf("a narrow record of a Mixed run: %+v", got)
	}
}

// An entry needing an undecided binding, or a team that cannot send, is held and named by
// what blocks it.
func TestTeamsHoldByWhatBlocks(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, nil)
	teams := NewTeams(cfg, TeamsDeps{})
	rec := relay.Record{Session: "s1", Placed: true, Placement: placed(claim.KindRepository, "github.com/acme/api"), At: time.Now()}
	if got := teams.Route(rec); got.Outcome != relay.Hold || got.Why != relay.WhyPending || got.Blocker != "pending:github.com/acme" {
		t.Fatalf("pending: %+v", got)
	}
	update(t, cfg, func(s *routing.State) {
		_ = s.Bind("github.com/acme", orgAcme, false)
		s.Teams[0].Refusals = 1
	})
	teams = NewTeams(cfg, TeamsDeps{})
	if got := teams.Route(rec); got.Outcome != relay.Hold || got.Why != relay.WhyBlocked || got.Blocker != "team:"+ids["acme"] {
		t.Fatalf("suspended: %+v", got)
	}
}

// A queued entry is rechecked against the state now: its own team while Active, waiting
// while it may recover, dropped once its enrollment ends or its run was Mixed by its time.
func TestTeamsRecheckAQueuedEntry(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, func(s *routing.State) { _ = s.Bind("github.com/acme", orgAcme, false) })
	at := time.Now()
	q := relay.Queued{Enrollment: ids["acme"], Project: "acme", Kind: string(routing.Repository), Repository: "github.com/acme/api", Session: "s1", At: at}
	if got := NewTeams(cfg, TeamsDeps{}).Recheck(q.Enrollment).Entry(q); got.Verdict != relay.SendNow {
		t.Fatalf("active: %+v", got)
	}
	if _, err := runs.Start(cfg.StateDir, "s1", at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := runs.MarkMixed(cfg.StateDir, "s1", at.Add(-time.Minute), runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}
	if got := NewTeams(cfg, TeamsDeps{}).Recheck(q.Enrollment).Entry(q); got.Verdict != relay.Discard || got.Why != relay.WhyMixed {
		t.Fatalf("its run Mixed before it: %+v", got)
	}
	before := q
	before.At = at.Add(-2 * time.Minute)
	if got := NewTeams(cfg, TeamsDeps{}).Recheck(before.Enrollment).Entry(before); got.Verdict != relay.SendNow {
		t.Fatalf("made before its run crossed: %+v", got)
	}
	update(t, cfg, func(s *routing.State) { s.Teams[0].Refusals = 1 })
	if got := NewTeams(cfg, TeamsDeps{}).Recheck(before.Enrollment).Entry(before); got.Verdict != relay.Wait {
		t.Fatalf("suspended: %+v", got)
	}
	update(t, cfg, func(s *routing.State) { s.Teams[0].Refusals = 2 })
	if got := NewTeams(cfg, TeamsDeps{}).Recheck(before.Enrollment).Entry(before); got.Verdict != relay.Discard || got.Why != relay.WhyBlocked {
		t.Fatalf("refused: %+v", got)
	}
	update(t, cfg, func(s *routing.State) { s.Deselect(ids["acme"]) })
	teams := NewTeams(cfg, TeamsDeps{})
	if got := teams.Recheck(before.Enrollment).Entry(before); got.Verdict != relay.Discard || got.Why != relay.WhyDeselected || teams.Selected(ids["acme"]) {
		t.Fatalf("unticked: %+v", got)
	}
}

// A pass over a queue judges its team once: while the team cannot send but may recover,
// every entry waits and none is judged; once it is Active, or Refused, each entry is. Each
// session's run ledger is read once in the pass, however many of its entries are queued.
func TestARecheckPassJudgesTheTeamOnceAndEachRunOnce(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, func(s *routing.State) { _ = s.Bind("github.com/acme", orgAcme, false) })
	at := time.Now()
	for _, s := range []string{"s1", "s2"} {
		if _, err := runs.Start(cfg.StateDir, s, at.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := runs.MarkMixed(cfg.StateDir, "s2", at.Add(-time.Minute), runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}
	pass := NewTeams(cfg, TeamsDeps{}).Recheck(ids["acme"])
	if v, waits := pass.TeamWaits(); waits {
		t.Fatalf("an Active team waits: %+v", v)
	}
	sent, dropped := 0, 0
	for i := range 1000 {
		q := relay.Queued{Enrollment: ids["acme"], Project: "acme", Kind: string(routing.Repository), Repository: "github.com/acme/api",
			Session: []string{"s1", "s2"}[i%2], At: at}
		switch pass.Entry(q).Verdict {
		case relay.SendNow:
			sent++
		case relay.Discard:
			dropped++
		}
	}
	if read := len(pass.(*recheck).runs); sent != 500 || dropped != 500 || read != 2 {
		t.Fatalf("1000 entries of two sessions: %d sent, %d dropped, %d ledgers read", sent, dropped, read)
	}
	for refusals, waits := range map[int]bool{1: true, 2: false} {
		update(t, cfg, func(s *routing.State) { s.Teams[0].Refusals = refusals })
		v, got := NewTeams(cfg, TeamsDeps{}).Recheck(ids["acme"]).TeamWaits()
		if got != waits || waits && (v.Verdict != relay.Wait || v.Why != relay.WhyBlocked || v.Blocker != "team:"+ids["acme"]) {
			t.Errorf("%d refusals: TeamWaits = %+v, %v", refusals, v, got)
		}
	}
}

// A migrated machine whose routing state is missing, unreadable or not yet committed sends
// nothing, and drops no queue.
func TestTeamsWithoutACommittedStateSendNothing(t *testing.T) {
	cfg := migratedMachine(t)
	check := func(name string) {
		t.Helper()
		teams := NewTeams(cfg, TeamsDeps{})
		rec := relay.Record{Placed: true, Placement: placed(claim.KindOutsideGit, "")}
		if got := teams.Route(rec); got.Outcome != relay.Hold || got.Why != relay.WhyNoState {
			t.Errorf("%s: Route = %+v", name, got)
		}
		pass := teams.Recheck("e1")
		if got, waits := pass.TeamWaits(); !waits || got.Verdict != relay.Wait || got.Why != relay.WhyNoState {
			t.Errorf("%s: TeamWaits = %+v, %v", name, got, waits)
		}
		if got := pass.Entry(relay.Queued{Enrollment: "e1"}); got.Verdict != relay.Wait {
			t.Errorf("%s: Recheck = %+v", name, got)
		}
		if _, err := teams.Resolve("e1", ""); err == nil {
			t.Errorf("%s: resolved a key", name)
		}
		if !teams.Selected("e1") || teams.UnknownToPersonal() {
			t.Errorf("%s: dropped a queue, or took unknown records", name)
		}
		if q, mapped := teams.Legacy("acme", config.Repository{}); !mapped || q.Enrollment != "" {
			t.Errorf("%s: a part queued before the migration = %+v, %v; want it waiting", name, q, mapped)
		}
	}
	check("missing")
	selectTeams(t, cfg, true, func(s *routing.State) { s.Migration.Phase = routing.Preparing })
	check("preparing")
	path, _ := routing.StatePath(cfg.StateDir, cfg.ProfileName)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("unreadable")
}

// A team is sent to with its own key from keys.v2.json alone: never the project's legacy
// key or an agent's. A missing key is minted, except for a Suspended or Refused team.
func TestTeamsResolveOnlyTheTeamsOwnKey(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, nil)
	if err := keystore.Set(cfg.Dir, "acme", staleKeyA, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(cfg.Dir, "claude", "acme", staleKeyA, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	var minted []string
	deps := TeamsDeps{AgentName: func(string) string { return "claude" }, RelayTargets: func(s []string) []string { return s },
		Mint: func(_ *config.Config, tm routing.Team) { minted = append(minted, tm.EnrollmentID) },
		User: func(_ *config.Config, tm routing.Team, endpoint string) string {
			return "user@" + tm.OrganizationID + "@" + endpoint
		}}
	if _, err := NewTeams(cfg, deps).Resolve(ids["acme"], "claude-code"); !errors.Is(err, relay.ErrNoKey) || !slices.Equal(minted, []string{ids["acme"]}) {
		t.Fatalf("Resolve = %v, minted %v: want no key and acme's minted", err, minted)
	}
	hosts := keystore.Hosts{OTLP: "https://otel.acme.example", API: "https://api.example"}
	if err := keystore.SetTeam(cfg.Dir, ids["acme"], "acme", teamKeyA, hosts); err != nil {
		t.Fatal(err)
	}
	pol, err := NewTeams(cfg, deps).Resolve(ids["acme"], "claude-code")
	if err != nil || pol.Key != teamKeyA || pol.Endpoint != "https://otel.acme.example" || !pol.IncludePrompts || pol.IncludeToolContent || !pol.RequireClaim {
		t.Fatalf("Resolve = %+v, %v", pol, err)
	}
	if pol.User != "user@"+orgAcme+"@https://otel.acme.example" {
		t.Fatalf("names %q: want the user signed in to acme's organization", pol.User)
	}
	pinned := deps
	pinned.OTLPPinned = true
	if pol, _ := NewTeams(cfg, pinned).Resolve(ids["acme"], "claude-code"); pol.Endpoint != cfg.OTLPURL {
		t.Fatalf("a pinned ingest went to %q", pol.Endpoint)
	}
	// An agent the developer did not choose sends nothing.
	other := deps
	other.AgentName = func(string) string { return "copilot" }
	if pol, _ := NewTeams(cfg, other).Resolve(ids["acme"], "copilot-cli"); pol.Signals == nil || len(pol.Signals) != 0 {
		t.Fatalf("an unchosen agent's policy = %+v", pol)
	}

	minted = nil
	if err := keystore.DeleteTeam(cfg.Dir, ids["acme"]); err != nil {
		t.Fatal(err)
	}
	for _, refusals := range []int{1, 2} {
		update(t, cfg, func(s *routing.State) { s.Teams[0].Refusals = refusals })
		if _, err := NewTeams(cfg, deps).Resolve(ids["acme"], ""); err == nil || len(minted) != 0 {
			t.Fatalf("%d refusals: %v, minted %v", refusals, err, minted)
		}
	}
}

// A team selected in another environment than the profile now points at is never sent to.
func TestTeamsRefuseATeamOfAnotherEnvironment(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, nil)
	if err := keystore.SetTeam(cfg.Dir, ids["me"], "me", teamKeyA, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	moved := *cfg
	moved.AuthURL = "https://auth.elsewhere"
	if _, err := NewTeams(&moved, TeamsDeps{}).Resolve(ids["me"], ""); !errors.Is(err, errOtherEnvironment) {
		t.Fatalf("Resolve = %v", err)
	}
}

// A machine not migrated never routes by teams.
func TestTeamsAreOffBeforeTheMigration(t *testing.T) {
	dir := t.TempDir()
	if NewTeams(&config.Config{Dir: dir, StateDir: dir, ProfileName: config.DefaultProfile}, TeamsDeps{}).Migrated() {
		t.Fatal("a machine with no credentials.v2.json routes by teams")
	}
}

// The refresher refreshes every enrollment in the routing state, whatever keys are kept
// here, and applies each result under the epoch its request started in.
func TestTheTeamRefresherRefreshesEveryEnrollment(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, nil)
	if err := keystore.Set(cfg.Dir, "some-old-team", staleKeyA, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	var asked []string
	d := Deps{LoadConfig: func() (*config.Config, error) { return cfg, nil },
		RefreshTeam: func(_ context.Context, _ *config.Config, tm routing.Team) (routing.Refresh, error) {
			asked = append(asked, tm.TeamID)
			pol := tm.Policy
			pol.Revision, pol.FetchedAt = tm.Policy.Revision+1, time.Now()
			return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &pol, At: time.Now()}, nil
		}}
	r := d.Refresher()
	if got := r.Teams(); !slices.Equal(got, []string{ids["acme"], ids["me"]}) {
		t.Fatalf("Teams = %v: want every enrollment, and no project for a key", got)
	}
	for _, id := range r.Teams() {
		if err := r.Refresh(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	st, _, _ := routing.Load(cfg.StateDir, cfg.ProfileName)
	if !slices.Equal(asked, []string{"acme", "me"}) || st.Teams[0].Policy.Revision != 2 || st.Teams[1].Policy.Revision != 2 {
		t.Fatalf("asked %v; state %+v", asked, st.Teams)
	}
	if got := r.Fetched(ids["acme"]); !got.Equal(st.Teams[0].Policy.FetchedAt) || r.every(ids["acme"]) != PolicyRefreshInterval {
		t.Fatalf("Fetched = %v, every %v", got, r.every(ids["acme"]))
	}
}

// A success already in flight when a refusal lands is discarded: it started in an epoch
// the refusal ended.
func TestTheTeamRefresherDiscardsASuccessFromAnEarlierEpoch(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, false, nil)
	d := Deps{LoadConfig: func() (*config.Config, error) { return cfg, nil },
		RefreshTeam: func(_ context.Context, _ *config.Config, tm routing.Team) (routing.Refresh, error) {
			// A refusal lands while this request is in flight.
			update(t, cfg, func(s *routing.State) {
				_, _ = s.ApplyRefresh(routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Failure: routing.Terminal})
			})
			pol := tm.Policy
			pol.Revision = 9
			return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &pol}, nil
		}}
	if err := d.Refresher().Refresh(t.Context(), ids["acme"]); err != nil {
		t.Fatal(err)
	}
	st, _, _ := routing.Load(cfg.StateDir, cfg.ProfileName)
	if tm := st.Teams[0]; tm.StateAt(time.Now()) != routing.Suspended || tm.Policy.Revision != 1 {
		t.Fatalf("team %+v: the refusal was undone", tm)
	}
}

// A refresh refused because the developer lost the organization makes the team Refused at
// once, published before its key is deleted; it is asked again once a day.
func TestTheTeamRefresherRefusesAndRechecksDaily(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, false, nil)
	if err := keystore.SetTeam(cfg.Dir, ids["acme"], "acme", teamKeyA, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	var failure atomic.Value
	failure.Store(routing.OrgAccessLost)
	d := Deps{LoadConfig: func() (*config.Config, error) { return cfg, nil },
		RefreshTeam: func(_ context.Context, _ *config.Config, tm routing.Team) (routing.Refresh, error) {
			return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Failure: failure.Load().(routing.ErrorClass), At: time.Now()}, errors.New("refused")
		}}
	r := d.Refresher()
	if err := r.Refresh(t.Context(), ids["acme"]); err == nil {
		t.Fatal("a refused refresh reported no error")
	}
	st, _, _ := routing.Load(cfg.StateDir, cfg.ProfileName)
	tm := st.Teams[0]
	if tm.StateAt(time.Now()) != routing.Refused || tm.RefusedAt.IsZero() || tm.LastError != routing.OrgAccessLost {
		t.Fatalf("team %+v: want Refused at once", tm)
	}
	if key, _, err := keystore.GetTeam(cfg.Dir, ids["acme"]); err != nil || key != "" {
		t.Fatalf("a Refused team keeps its key: %q, %v", key, err)
	}
	if got := r.Fetched(ids["acme"]); !got.Equal(tm.RefusedAt) || r.every(ids["acme"]) != RefusedRecheck {
		t.Fatalf("a Refused team is due %v after %v", r.every(ids["acme"]), got)
	}
	// The daily recheck brings its policy back: Active again under the same enrollment.
	d.RefreshTeam = func(_ context.Context, _ *config.Config, tm routing.Team) (routing.Refresh, error) {
		pol := tm.Policy
		pol.FetchedAt = time.Now()
		return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &pol}, nil
	}
	if err := d.Refresher().Refresh(t.Context(), ids["acme"]); err != nil {
		t.Fatal(err)
	}
	st, _, _ = routing.Load(cfg.StateDir, cfg.ProfileName)
	if tm := st.Teams[0]; tm.EnrollmentID != ids["acme"] || tm.StateAt(time.Now()) != routing.Active {
		t.Fatalf("after the recheck: %+v", tm)
	}
}

// Before the migration the refresher lists the teams it always did, from keys.
func TestTheRefresherBeforeTheMigrationListsKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Dir: dir, StateDir: dir, ProfileName: config.DefaultProfile}
	if err := keystore.Set(dir, "p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	selectTeams(t, cfg, false, nil) // a routing state no unmigrated relay reads
	r := Deps{LoadConfig: func() (*config.Config, error) { return cfg, nil }}.Refresher()
	if got := r.Teams(); !slices.Equal(got, []string{"p1"}) {
		t.Fatalf("Teams = %v", got)
	}
}

// A team's key is created once, under the machine's mint lock, and never over one already
// on file.
func TestMintTeamKeyCreatesOnce(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, false, nil)
	created := 0
	create := func(_ context.Context, _ *config.Config, tm routing.Team) (string, error) {
		created++
		return teamKeyA, nil
	}
	tm := routing.Team{EnrollmentID: ids["acme"], TeamID: "acme"}
	for range 2 {
		if err := MintTeamKey(t.Context(), cfg, tm, create); err != nil {
			t.Fatal(err)
		}
	}
	if key, _, err := keystore.GetTeam(cfg.Dir, ids["acme"]); err != nil || key != teamKeyA || created != 1 {
		t.Fatalf("key %q, %v; created %d times", key, err, created)
	}
}

// The relay mints no key for a Suspended or Refused team, and takes back one whose creation
// finished after the team was refused.
func TestTheTeamMinterNeverKeysARefusedTeam(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, false, nil)
	var created atomic.Int32
	m := NewTeamKeyMinter(t.Context(), cfg, func(_ context.Context, _ *config.Config, tm routing.Team) (string, error) {
		created.Add(1)
		// The team is refused while its key is created.
		update(t, cfg, func(s *routing.State) { s.Teams[0].Refusals = 2 })
		return teamKeyA, nil
	})
	st, _, _ := routing.Load(cfg.StateDir, cfg.ProfileName)
	if m.mintNow(cfg, st.Teams[0]) {
		t.Fatal("a key minted for a team refused meanwhile was kept")
	}
	if key, _, _ := keystore.GetTeam(cfg.Dir, ids["acme"]); key != "" || created.Load() != 1 {
		t.Fatalf("key %q after %d creations", key, created.Load())
	}
	if m.mintNow(cfg, st.Teams[0]) || created.Load() != 1 {
		t.Fatalf("a Refused team was given a key: %d creations", created.Load())
	}
}

// sessionKeys is an agent naming its session by session.id.
type sessionKeys struct{}

func (sessionKeys) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

// End to end on a migrated machine: a hook's location-only placement routes the session's
// records to the team bound there, with that team's key alone and its project stamped;
// another session's, outside git, to personal with personal's.
func TestAMigratedRelaySendsEachSessionToItsTeam(t *testing.T) {
	cfg := migratedMachine(t)
	ids := selectTeams(t, cfg, true, func(s *routing.State) { _ = s.Bind("github.com/acme", orgAcme, false) })
	var mu sync.Mutex
	got := map[string]string{} // key → project stamped
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m logspb.LogsData
		_ = proto.Unmarshal(body, &m)
		project := ""
		for _, kv := range m.GetResourceLogs()[0].GetResource().GetAttributes() {
			if kv.GetKey() == semconv.MiradorProjectIDKey {
				project = kv.GetValue().GetStringValue()
			}
		}
		mu.Lock()
		got[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] = project
		mu.Unlock()
	}))
	defer ingest.Close()
	cfg.OTLPURL = ingest.URL
	for team, key := range map[string]string{"acme": teamKeyA, "me": mintedKey} {
		if err := keystore.SetTeam(cfg.Dir, ids[team], team, key, keystore.Hosts{OTLP: ingest.URL}); err != nil {
			t.Fatal(err)
		}
	}
	// The project's key from before the migration is never used.
	if err := keystore.Set(cfg.Dir, "acme", staleKeyA, keystore.Hosts{OTLP: ingest.URL}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claim.Write(cfg.StateDir, "s-acme", claim.Claim{Kind: claim.KindRepository, Tool: "claude-code", Repository: config.Repository{Origin: "github.com/acme/api"}}, now)
	claim.Write(cfg.StateDir, "s-side", claim.Claim{Kind: claim.KindOutsideGit, Tool: "claude-code", Cwd: "/tmp/scratch"}, now)
	d := Deps{Correlators: []shape.Correlator{sessionKeys{}}, AgentName: func(string) string { return "claude" }, RelayTargets: func(s []string) []string { return s }}
	opts := d.Engine(t.Context(), cfg.StateDir, cfg, Settings{Hold: time.Minute}, nil)
	opts.Token, opts.HeartbeatSend = "tok", nil
	r := relay.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	for _, session := range []string{"s-acme", "s-side"} {
		m := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(now.Add(time.Second).UnixNano()), Attributes: []*commonpb.KeyValue{{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: session}}}}}}}}}}}
		body, _ := proto.Marshal(m)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[teamKeyA] != "acme" || got[mintedKey] != "me" {
		t.Fatalf("sent by key %v; counters %v", got, r.Stats().Snapshot().Counters)
	}
}

// The relay routes each record by the profile its session's hooks placed it under, so a
// switch of the active profile while it runs (terma config use) never sends a session
// placed under the new profile with the old one's teams. Here the relay started under the
// default profile, where Acme's checkouts go to Acme; client selects personal alone. A
// record placed under client goes by client once it is active; one whose placement names
// no profile, one with no placement, and a session still placed under the default profile
// go on by the relay's start profile, whichever is active. Each enrollment is resolved and
// rechecked under its own profile.
func TestASwitchOfProfileWhileTheRelayRunsRoutesByTheSessionsProfile(t *testing.T) {
	cfg := migratedMachine(t)
	work := selectTeams(t, cfg, true, func(s *routing.State) {
		_ = s.Bind("github.com/acme", orgAcme, false)
		s.Migration.Legacy = map[string]string{"legacy-project": s.Teams[0].EnrollmentID}
	})
	client := *cfg
	client.ProfileName = "client"
	if err := config.UpdateProfile(cfg.Dir, client.ProfileName, func(p *config.Profile) { p.Harnesses = []string{"claude"} }); err != nil {
		t.Fatal(err)
	}
	other := selectTeams(t, &client, true, func(s *routing.State) {
		s.Deselect(s.Teams[0].EnrollmentID)
		s.Migration.Legacy = map[string]string{"legacy-project": s.Teams[0].EnrollmentID}
	})
	var active atomic.Value
	active.Store(config.DefaultProfile)
	teams := NewTeams(cfg, TeamsDeps{Active: func() string { return active.Load().(string) }})
	teams.ttl = 0
	acme := func(profile string) relay.Record {
		p := placed(claim.KindRepository, "github.com/acme/api")
		p.Profile = profile
		return relay.Record{Session: "s1", Placed: true, Placement: p, At: time.Now()}
	}
	if got := teams.Route(acme(config.DefaultProfile)); got.Outcome != relay.Send || got.Queued.Enrollment != work["acme"] {
		t.Fatalf("before the switch: %+v", got)
	}
	unplaced := relay.Record{Session: "s2", At: time.Now()}
	before := teams.Route(unplaced)

	active.Store(client.ProfileName)
	for name, rec := range map[string]relay.Record{
		"placed under client":          acme(client.ProfileName),
		"outside git, placed under it": {Session: "s3", Placed: true, Placement: claim.Claim{Kind: claim.KindOutsideGit, Profile: client.ProfileName}, At: time.Now()},
	} {
		got := teams.Route(rec)
		if got.Outcome != relay.Send || got.Queued.Enrollment != other["me"] {
			t.Errorf("%s after the switch: %+v, want client's personal %s", name, got, other["me"])
		}
		for _, old := range work {
			if got.Queued.Enrollment == old {
				t.Errorf("%s after the switch went to the default profile's %s", name, old)
			}
		}
	}
	for name, rec := range map[string]relay.Record{
		"placed under the default profile": acme(config.DefaultProfile),
		"placed under no profile":          acme(""),
	} {
		if got := teams.Route(rec); got.Outcome != relay.Send || got.Queued.Enrollment != work["acme"] {
			t.Errorf("%s after the switch: %+v, want the start profile's Acme %s", name, got, work["acme"])
		}
	}
	if got := teams.Route(unplaced); got.Outcome != before.Outcome || got.Why != before.Why || got.Queued.Enrollment != before.Queued.Enrollment {
		t.Errorf("a record with no placement after the switch: %+v, want it routed as before, %+v", got, before)
	}
	// A part queued before the migration names no profile: the start profile maps it.
	if q, mapped := teams.Legacy("legacy-project", config.Repository{}); !mapped || q.Enrollment != work["acme"] {
		t.Errorf("Legacy after the switch = %+v, %v; want the start profile's %s", q, mapped, work["acme"])
	}

	for _, e := range []string{work["acme"], other["me"]} {
		if !teams.Selected(e) {
			t.Errorf("%s is not taken for selected", e)
		}
		if _, err := teams.Resolve(e, ""); !errors.Is(err, relay.ErrNoKey) {
			t.Errorf("Resolve(%s) = %v, want it found and waiting for its key", e, err)
		}
		q := relay.Queued{Enrollment: e, Kind: string(routing.OutsideGit), Session: "s3", At: time.Now()}
		if e == work["acme"] {
			q.Kind, q.Repository = string(routing.Repository), "github.com/acme/api"
		}
		if got := teams.Recheck(e).Entry(q); got.Verdict != relay.SendNow {
			t.Errorf("Recheck(%s) = %+v", e, got)
		}
	}
	if teams.Selected("enrollment-of-no-profile") {
		t.Error("an enrollment no profile selects is taken for selected")
	}
}
