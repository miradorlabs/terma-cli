package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// migratedAuth is the sign-in environment of every migrated test team.
const migratedAuth = "https://auth.example"

// migrateForTest marks testApp's machine migrated and gives the default profile a routing
// state selecting Acme Platform, with github.com/acme bound to Acme and the legacy project
// mapped to its cutover enrollment, which it returns.
func migrateForTest(t *testing.T) string {
	t.Helper()
	if err := os.WriteFile(config.CredentialsV2Path(testApp.dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var acme string
	_, err := routing.Create(testApp.stateDir, config.DefaultProfile, func(s *routing.State) error {
		var err error
		acme, err = s.Select(routing.Team{OrganizationID: "org-acme", AuthURL: migratedAuth, TeamID: routeProdProject, Policy: config.Policy{
			Mode: config.ModeRepo, Repositories: []string{"github.com/acme/*"}, Wildcards: true, Revision: 1,
			TeamID: routeProdProject, OrganizationID: "org-acme", AuthURL: migratedAuth, FetchedAt: time.Now(),
		}})
		if err != nil {
			return err
		}
		s.Migration = &routing.Migration{Phase: routing.Committed, Legacy: map[string]string{routeProdProject: acme}}
		return s.Bind("github.com/acme", "org-acme", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	return acme
}

// `terma spool flush` on a migrated machine sends each event to its own enrollment with
// that enrollment's key, an event queued before the migration to its cutover enrollment,
// and drops what its team no longer collects, never sending it elsewhere.
func TestSpoolFlushOnAMigratedMachineSendsByEnrollment(t *testing.T) {
	routingSandbox(t)
	acme := migrateForTest(t)
	host := newIngestHost(t, "ter_srv_acme")
	legacyHost := newIngestHost(t, "ter_srv_legacy")
	if err := keystore.SetTeam(testApp.dir, acme, routeProdProject, "ter_srv_acme", keystore.Hosts{OTLP: host.URL}); err != nil {
		t.Fatal(err)
	}
	routeProject(t, routeProdProject, "ter_srv_legacy", legacyHost.URL)
	s := spoolForTest(t)
	routed := func(origin string) spool.Event {
		return spool.Event{Name: semconv.TermaSessionStartEvent, SessionID: "s1", Time: time.Now(),
			Repository: config.Repository{Origin: origin}, Profile: config.DefaultProfile, Enrollment: acme}
	}
	if err := s.Append(routed("github.com/acme/app"), routed("github.com/dawson/side")); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, s, routeProdProject, time.Now())

	out, err := runTerma(t, "spool", "flush")
	if err != nil {
		t.Fatalf("flush: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Flushed 2 events, dropped 1 their team no longer collects") {
		t.Fatalf("unexpected report:\n%s", out)
	}
	if seen := host.keysSeen(); len(seen) == 0 || seen[0] != "Bearer ter_srv_acme" {
		t.Fatalf("the enrollment's host saw %v", seen)
	}
	if seen := legacyHost.keysSeen(); len(seen) != 0 {
		t.Fatalf("the legacy key's host saw %v", seen)
	}
	if n, _, _ := s.Pending(); n != 0 {
		t.Fatalf("pending = %d", n)
	}
}

// A hook that found its policy stale starts a flush to refresh it. Once the machine is
// migrated that single-team refresh never runs, even for a team an older binary wrote back
// into config.json: it would fetch and store outside the routing state.
func TestAMigratedFlushNeverRefreshesTheProfilesPolicy(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		useConfigDir(t, t.TempDir())
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
			p.OrganizationID, p.Team = "org-test", "legacy-team"
		}); err != nil {
			t.Fatal(err)
		}
		stale := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/app"}, TeamID: "legacy-team",
			OrganizationID: "org-test", AuthURL: cfg.AuthURL, FetchedAt: time.Now().Add(-time.Hour).UTC()}
		if err := config.WritePolicy(testApp.stateDir, stale); err != nil {
			t.Fatal(err)
		}
		if migrated {
			migrateForTest(t)
		}
		spoolForTest(t)
		if _, err := testApp.flushSpool(context.Background(), true, 0); err != nil {
			t.Fatal(err)
		}
		got, _, err := config.ReadPolicy(testApp.stateDir, "legacy-team")
		if err != nil {
			t.Fatal(err)
		}
		if refreshed := got.FetchedAt.After(stale.FetchedAt); refreshed == migrated {
			t.Fatalf("migrated %v: refreshed %v", migrated, refreshed)
		}
	}
}

// A migrated pass names each reason an event was held or dropped once, and a missing key
// points at doctor, which names the team; what Mixed runs dropped, it counts by why.
func TestDescribeFlushOnAMigratedMachine(t *testing.T) {
	_, undelivered := describeFlush(delivery.Result{Migrated: true, Held: 6, Blocked: 3, Unrouted: 1, Discarded: 4, Mixed: 5,
		MixedBy: map[runs.Reason]int{runs.Crossed: 1, runs.UnknownReach: 4}})
	want := []string{
		"holding 2 for a team key (run `terma doctor`)",
		"holding 3 until their team can send again (run `terma doctor`)",
		"holding 1 until this machine's routing state can be read (run `terma doctor`)",
		"dropped 4 their team no longer collects",
		"dropped 5 because their session's run is Mixed (4 ran a shell command whose reach is not known, 1 crossed organizations)",
	}
	if strings.Join(undelivered, "|") != strings.Join(want, "|") {
		t.Errorf("undelivered =\n  %q\nwant\n  %q", undelivered, want)
	}
}

// A flush whose only drops are by policy, an event its team no longer collects and one
// from a session that crossed organizations, says how many of each it dropped and exits 0,
// as one that withholds an event by its policy always has; one that drops an event naming
// no team has lost it, and exits incomplete.
func TestAFlushThatDropsOnlyByPolicySucceeds(t *testing.T) {
	routingSandbox(t)
	acme := migrateForTest(t)
	host := newIngestHost(t, "ter_srv_acme")
	if err := keystore.SetTeam(testApp.dir, acme, routeProdProject, "ter_srv_acme", keystore.Hosts{OTLP: host.URL}); err != nil {
		t.Fatal(err)
	}
	s := spoolForTest(t)
	event := func(session, origin string) spool.Event {
		return spool.Event{Name: semconv.TermaToolCallEvent, SessionID: session, Time: time.Now(),
			Repository: config.Repository{Origin: origin}, Profile: config.DefaultProfile, Enrollment: acme}
	}
	if err := runs.MarkMixed(testApp.stateDir, "crossed", time.Now().Add(-time.Minute), runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(event("s1", "github.com/acme/app"), event("s1", "github.com/dawson/side"), event("crossed", "github.com/acme/app")); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "spool", "flush")
	if err != nil {
		t.Fatalf("a flush that dropped only by policy failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dropped 1 their team no longer collects") || !strings.Contains(out, "dropped 1 because their session's run is Mixed (1 crossed organizations)") {
		t.Fatalf("the report does not name both drops:\n%s", out)
	}
	if res := (delivery.Result{Discarded: 1, Mixed: 1}); res.Lost() {
		t.Error("a drop by policy counts as loss")
	}

	appendEvent(t, s, "", time.Now())
	out, err = runTerma(t, "spool", "flush")
	if code, ok := exitCodeOf(err); !ok || code != ExitIncomplete {
		t.Fatalf("an event naming no team: err=%v code=%d\n%s", err, code, out)
	}
}

// Setup never mints a key on a migrated machine, least of all into keys.json, which an
// older binary sends with: it reports each team's own key, which the relay creates.
func TestEnsureSpoolKeyNeverMintsOnceMigrated(t *testing.T) {
	routingSandbox(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CredentialsV2Path(testApp.dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k := testApp.ensureSpoolKey(context.Background(), cfg); !strings.Contains(k.fix, "terma doctor") {
		t.Fatalf("with no routing state: %+v", k)
	}
	acme := migrateForTest(t)
	k := testApp.ensureSpoolKey(context.Background(), cfg)
	if !strings.Contains(k.state, routeProdProject) || k.fix == "" {
		t.Fatalf("with the team's key missing: %+v", k)
	}
	if _, err := os.Stat(keystore.FilePath(testApp.dir)); !os.IsNotExist(err) {
		t.Fatalf("keys.json was written: %v", err)
	}
	if err := keystore.SetTeam(testApp.dir, acme, routeProdProject, "ter_srv_acme", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if k := testApp.ensureSpoolKey(context.Background(), cfg); k.fix != "" {
		t.Fatalf("with the team's key filed: %+v", k)
	}
}

// On a migrated machine with no team ticked, setup's key step sends the developer to
// `terma teams`, where teams are ticked.
func TestEnsureSpoolKeyWithNoTeamSaysTermaTeams(t *testing.T) {
	routingSandbox(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CredentialsV2Path(testApp.dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.Create(testApp.stateDir, cfg.ProfileName, func(*routing.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if k := testApp.ensureSpoolKey(context.Background(), cfg); !strings.Contains(k.fix, "`terma teams`") || strings.Contains(k.fix, "terma setup") {
		t.Fatalf("with no team ticked: %+v", k)
	}
}

// A migrated hook event names the profile's signed-in user only for a team of the
// organization it is signed in to, going to the profile's own ingest.
func TestEnrolledUserNamesOnlyTheSignedInOrganizationsUser(t *testing.T) {
	routingSandbox(t)
	migrateForTest(t)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, &auth.Credential{AccessToken: "token", OrganizationID: "org-acme",
		AuthURL: migratedAuth, UserID: "user-1", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	own := "http://127.0.0.1:1"
	acme := routing.Team{OrganizationID: "org-acme", AuthURL: migratedAuth, TeamID: routeProdProject}
	other := routing.Team{OrganizationID: "org-other", AuthURL: migratedAuth, TeamID: routeDevProject}
	for _, tc := range []struct {
		name     string
		team     routing.Team
		hosts    keystore.Hosts
		endpoint string
		want     string
	}{
		{"its organization's team", acme, keystore.Hosts{}, own, "user-1"},
		{"another organization's team", other, keystore.Hosts{}, own, ""},
		{"another ingest", acme, keystore.Hosts{}, "https://otel.elsewhere", ""},
		{"a key filed with another API host", acme, keystore.Hosts{API: "https://api.elsewhere"}, own, ""},
	} {
		if got := testApp.enrolledUser(config.DefaultProfile, tc.team, tc.hosts, tc.endpoint); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// twoOrganizations makes testApp's machine migrated, its default profile selecting a team
// of org-a collecting github.com/a/* and one of org-b collecting github.com/b/*, each owner
// bound to its organization, and signed in to both organizations, org-a active. It returns
// the two enrollments.
func twoOrganizations(t *testing.T) (a, b string) {
	t.Helper()
	if err := os.WriteFile(config.CredentialsV2Path(testApp.dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	team := func(org, id, owner string) routing.Team {
		return routing.Team{OrganizationID: org, AuthURL: migratedAuth, TeamID: id, Policy: config.Policy{
			Mode: config.ModeRepo, Repositories: []string{owner + "/*"}, Wildcards: true, Revision: 1,
			TeamID: id, OrganizationID: org, AuthURL: migratedAuth, FetchedAt: time.Now(),
		}}
	}
	_, err := routing.Create(testApp.stateDir, config.DefaultProfile, func(s *routing.State) error {
		var err error
		if a, err = s.Select(team("org-a", "team-a", "github.com/a")); err != nil {
			return err
		}
		if b, err = s.Select(team("org-b", "team-b", "github.com/b")); err != nil {
			return err
		}
		s.Migration = &routing.Migration{Phase: routing.Committed}
		if err := s.Bind("github.com/a", "org-a", false); err != nil {
			return err
		}
		return s.Bind("github.com/b", "org-b", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org-b", "org-a"} {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, &auth.Credential{AccessToken: "ter_cli_" + org,
			OrganizationID: org, AuthURL: migratedAuth, UserID: "user-" + strings.TrimPrefix(org, "org-"), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// On a migrated machine signed in to two organizations, each team's spooled records name the
// user the sign-in to that team's organization names, the active organization's or not.
func TestAFlushNamesEachOrganizationsUser(t *testing.T) {
	routingSandbox(t)
	var mu sync.Mutex
	sent := map[string]string{} // each key's request bodies, as the ingest accepted them
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		sent[r.Header.Get("Authorization")] += string(body)
	}))
	defer srv.Close()
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.OTLPURL = srv.URL }); err != nil {
		t.Fatal(err)
	}
	a, b := twoOrganizations(t)
	for enrollment, key := range map[string]string{a: "ter_srv_a", b: "ter_srv_b"} {
		team := map[string]string{a: "team-a", b: "team-b"}[enrollment]
		if err := keystore.SetTeam(testApp.dir, enrollment, team, key, keystore.Hosts{OTLP: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	s := spoolForTest(t)
	for enrollment, origin := range map[string]string{a: "github.com/a/app", b: "github.com/b/app"} {
		if err := s.Append(spool.Event{Name: semconv.TermaSessionStartEvent, SessionID: "s1", Time: time.Now(),
			Repository: config.Repository{Origin: origin}, Profile: config.DefaultProfile, Enrollment: enrollment}); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := runTerma(t, "spool", "flush"); err != nil {
		t.Fatalf("flush: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("the ingest was sent with the keys of %v", sent)
	}
	for key, c := range map[string]struct{ user, other string }{"ter_srv_a": {"user-a", "user-b"}, "ter_srv_b": {"user-b", "user-a"}} {
		body := sent["Bearer "+key]
		if !strings.Contains(body, semconv.TermaUserIDKey) || !strings.Contains(body, `"stringValue":"`+c.user+`"`) ||
			strings.Contains(body, `"stringValue":"`+c.other+`"`) {
			t.Errorf("with %s the ingest was sent, wanting %s:\n%s", key, c.user, body)
		}
	}
}

// enrolledUser reads the credentials index alone, never the keychain, and a profile set up
// with a server key names no one, signed in to the team's organization or not.
func TestEnrolledUserNeverReadsTheKeychainNorNamesAServerKeysUser(t *testing.T) {
	routingSandbox(t)
	twoOrganizations(t)
	own := "http://127.0.0.1:1"
	b := routing.Team{OrganizationID: "org-b", AuthURL: migratedAuth, TeamID: "team-b"}
	secret.FailForTest(t, testApp.dir)
	if got := testApp.enrolledUser(config.DefaultProfile, b, keystore.Hosts{}, own); got != "user-b" {
		t.Fatalf("with the keychain locked, the inactive organization's team names %q, want user-b", got)
	}
	if got := testApp.enrolledUser(config.DefaultProfile, routing.Team{OrganizationID: "org-b", AuthURL: "https://auth.other", TeamID: "team-b"},
		keystore.Hosts{}, own); got != "" {
		t.Errorf("a team of another environment names %q", got)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.ServerKeySignIn = true }); err != nil {
		t.Fatal(err)
	}
	if got := testApp.enrolledUser(config.DefaultProfile, b, keystore.Hosts{}, own); got != "" {
		t.Fatalf("a profile set up with a server key names %q", got)
	}
}

// An unmigrated machine's flush ignores routing state and team keys on disk, as before.
func TestAnUnmigratedFlushIgnoresRoutingState(t *testing.T) {
	routingSandbox(t)
	host := newIngestHost(t, "ter_srv_legacy")
	routeProject(t, routeProdProject, "ter_srv_legacy", host.URL)
	s := spoolForTest(t)
	e := spool.Event{Name: "terma.commit", Time: time.Now(), Repository: appRepo,
		Attrs: map[string]any{hookrun.AttrProjectID: routeProdProject}, Profile: config.DefaultProfile, Enrollment: "an-enrollment"}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	res, err := testApp.flushSpool(context.Background(), true, 0)
	if err != nil || res.Migrated || res.Sent != 1 {
		t.Fatalf("flushSpool = %+v, %v", res, err)
	}
	if seen := host.keysSeen(); len(seen) != 1 || seen[0] != "Bearer ter_srv_legacy" {
		t.Fatalf("the legacy host saw %v", seen)
	}
}
