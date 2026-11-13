package migrate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newMachine is an empty machine: its own config and state directories.
func newMachine(t *testing.T) Machine {
	t.Helper()
	return Machine{ConfigDir: t.TempDir(), StateDir: t.TempDir(), FlushWait: time.Second}
}

// legacy is one profile as terma before multi-org routing leaves it: its team in
// config.json, the team's validated policy, a sign-in to its organization and the team's
// key in keys.json.
type legacy struct {
	profile, org, team string
	entries            []string
	wildcards, global  bool
	// serverKey: signed in with the team's server key (TERMA_API_KEY), not a login.
	serverKey bool
	// noPolicy: no policy was ever validated for the team.
	noPolicy bool
	key      string
}

// acme, clientco and bare are the profiles of the machines most tests migrate: two
// teams of two organizations, and a profile with no team.
var (
	acme     = legacy{profile: "default", org: "acme", team: "acme-platform", entries: []string{"github.com/acme/api", "gitlab.com/acme/platform/web"}, key: "ter_srv_acme"}
	clientco = legacy{profile: "work", org: "clientco", team: "clientco-contractors", entries: []string{"github.com/clientco/app"}, key: "ter_srv_clientco"}
	bare     = legacy{profile: "bare", org: "me"}
)

// setUp writes l onto the machine as today's terma would.
func (m Machine) setUp(t *testing.T, l legacy) {
	t.Helper()
	must(t, config.UpdateProfile(m.ConfigDir, l.profile, func(p *config.Profile) {
		p.OrganizationID, p.OrganizationName = l.org, l.org+" Inc"
		p.Team, p.TeamName = l.team, cond(l.team != "", l.team+" name", "")
		p.Harnesses = []string{"claude"}
		p.ServerKeySignIn = l.serverKey
	}))
	authURL := m.authURL(t, l.profile)
	if l.team != "" && !l.noPolicy {
		mode := config.ModeRepo
		if l.global {
			mode = config.ModeGlobal
		}
		must(t, config.WritePolicy(m.StateDir, config.Policy{Mode: mode, Repositories: l.entries, Wildcards: l.wildcards,
			GitHooks: true, IncludePrompts: true, TeamID: l.team, OrganizationID: l.org, AuthURL: authURL, Revision: 3,
			FetchedAt: time.Now().Add(-time.Hour)}))
	}
	if !l.serverKey && l.org != "" {
		_, err := auth.SaveCredential(m.ConfigDir, l.profile, &auth.Credential{AccessToken: "ter_cli_" + l.profile,
			RefreshToken: "ter_clr_" + l.profile, ExpiresAt: time.Now().Add(time.Hour), SessionID: "s-" + l.profile,
			OrganizationID: l.org, AuthURL: authURL})
		must(t, err)
	}
	if l.key != "" {
		must(t, keystore.Set(m.ConfigDir, l.team, l.key, keystore.Hosts{OTLP: "https://otel." + l.org + ".example"}))
	}
}

func cond(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

// authURL is the environment profile resolves to, which fetched its policies.
func (m Machine) authURL(t *testing.T, profile string) string {
	t.Helper()
	cfg, err := config.Load(m.ConfigDir, m.StateDir, config.Overrides{Profile: profile})
	must(t, err)
	return cfg.AuthURL
}

// commit migrates m, failing the test unless it commits.
func (m Machine) commit(t *testing.T) {
	t.Helper()
	out, err := Run(context.Background(), m)
	if err != nil || out != Committed {
		t.Fatalf("Run = %s, %v", out, err)
	}
}

// state is profile's routing state, which must be there.
func (m Machine) state(t *testing.T, profile string) routing.State {
	t.Helper()
	s, ok, err := routing.Load(m.StateDir, profile)
	if err != nil || !ok {
		t.Fatalf("routing state of %s: %v, %v", profile, ok, err)
	}
	return s
}

// migrated checks that l came through the migration: its routing state Committed with its
// team as the one enrollment, its legacy project mapped to it, its key under it in
// keys.v2.json, its sign-in in credentials.v2.json, and nothing of it left in the legacy
// files. It returns the enrollment.
func (m Machine) migrated(t *testing.T, l legacy) string {
	t.Helper()
	s := m.state(t, l.profile)
	if s.Migration == nil || s.Migration.Phase != routing.Committed {
		t.Fatalf("%s's migration record = %+v", l.profile, s.Migration)
	}
	if l.team == "" {
		if len(s.Teams) != 0 || len(s.Migration.Legacy) != 0 || len(s.Bindings) != 0 {
			t.Fatalf("a profile with no team has %+v", s)
		}
		return ""
	}
	if len(s.Teams) != 1 {
		t.Fatalf("%s's teams = %+v", l.profile, s.Teams)
	}
	tm := s.Teams[0]
	if tm.TeamID != l.team || tm.OrganizationID != l.org || tm.AuthURL != m.authURL(t, l.profile) || tm.Personal || tm.EnrollmentID == "" {
		t.Fatalf("%s's team = %+v", l.profile, tm)
	}
	if got := s.Migration.Legacy; len(got) != 1 || got[l.team] != tm.EnrollmentID {
		t.Fatalf("%s's legacy map = %v", l.profile, got)
	}
	if l.key != "" {
		key, hosts, err := keystore.GetTeam(m.ConfigDir, tm.EnrollmentID)
		if err != nil || key != l.key || hosts.OTLP != "https://otel."+l.org+".example" {
			t.Fatalf("%s's team key in keys.v2.json = %q, %+v, %v", l.profile, key, hosts, err)
		}
	}
	if old, err := keystore.Get(m.ConfigDir, l.team); err != nil || old != "" {
		t.Fatalf("keys.json still holds %s's key: %q, %v", l.team, old, err)
	}
	if !l.serverKey && l.org != "" {
		cred, err := auth.LoadCredentialFor(m.ConfigDir, l.profile, l.org)
		if err != nil || cred.AccessToken != "ter_cli_"+l.profile {
			t.Fatalf("%s's sign-in after the migration = %+v, %v", l.profile, cred, err)
		}
	}
	file, err := config.LoadFile(m.ConfigDir)
	must(t, err)
	if p := file.Profiles[l.profile]; p == nil || p.Team != "" || p.TeamName != "" || p.OrganizationID != l.org || len(p.Harnesses) != 1 {
		t.Fatalf("%s in config.json = %+v", l.profile, p)
	}
	return tm.EnrollmentID
}

// legacyRetired checks that no older terma finds a team, a key or a sign-in.
func (m Machine) legacyRetired(t *testing.T) {
	t.Helper()
	if projects := keystore.CollectionProjects(m.ConfigDir); len(projects) != 0 {
		t.Fatalf("keys.json still indexes %v", projects)
	}
	b, err := os.ReadFile(config.CredentialsPath(m.ConfigDir))
	must(t, err)
	if strings.Contains(string(b), "organization_id") {
		t.Fatalf("credentials.json still holds sign-ins:\n%s", b)
	}
	if !config.Migrated(m.ConfigDir) {
		t.Fatal("the machine is not marked migrated")
	}
}

// Several profiles migrate at once: each gets its own enrollment for its own team, with
// its own key, sign-in and bindings, and a profile with no team an empty list.
func TestRunMigratesEveryProfile(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	wild := legacy{profile: "wild", org: "wildco", team: "wild-team", entries: []string{"github.com/wildco/*", "github.com/wildco/api"}, wildcards: true, key: "ter_srv_wild"}
	for _, l := range []legacy{acme, clientco, bare, wild} {
		m.setUp(t, l)
	}
	if !Needed(m.ConfigDir, m.StateDir) {
		t.Fatal("an unmigrated machine needs no migration")
	}
	m.commit(t)
	enrollments := map[string]bool{}
	for _, l := range []legacy{acme, clientco, bare, wild} {
		enrollments[m.migrated(t, l)] = true
	}
	if len(enrollments) != 4 {
		t.Fatalf("enrollments are not one per profile: %v", enrollments)
	}
	m.legacyRetired(t)

	// Bindings: a repository binding for each exact entry, an owner binding for a wildcard.
	want := map[string]map[string]routing.Binding{
		"default": {"github.com/acme/api": {Organization: "acme"}, "gitlab.com/acme/platform/web": {Organization: "acme"}},
		"work":    {"github.com/clientco/app": {Organization: "clientco"}},
		"wild":    {"github.com/wildco": {Organization: "wildco"}, "github.com/wildco/api": {Organization: "wildco"}},
	}
	for profile, bindings := range want {
		if got := m.state(t, profile).Bindings; !mapsEqual(got, bindings) {
			t.Errorf("%s's bindings = %v, want %v", profile, got, bindings)
		}
	}
	if _, ok := ReadBlock(m.StateDir); ok {
		t.Fatal("a migrated machine keeps a record of why it was not")
	}

	// Done once: a later start changes nothing.
	if Needed(m.ConfigDir, m.StateDir) {
		t.Fatal("a migrated machine still needs migrating")
	}
	before := snapshot(t, m)
	if out, err := Run(context.Background(), m); err != nil || out != Done {
		t.Fatalf("a second Run = %s, %v", out, err)
	}
	if after := snapshot(t, m); !mapsEqual(before, after) {
		t.Fatal("a second Run changed the machine")
	}
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// snapshot is every file the migration reads or writes, by path, without lock files, the
// spool's folder or the record for doctor.
func snapshot(t *testing.T, m Machine) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range []string{m.ConfigDir, m.StateDir} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, ".lock") || d.Name() == BlockFile || strings.Contains(path, "/spool/") {
				return err
			}
			b, err := os.ReadFile(path)
			out[strings.TrimPrefix(path, root)] = string(b)
			return err
		})
		must(t, err)
	}
	return out
}

// A machine nothing was ever set up on migrates to an empty one, marked migrated.
func TestRunMigratesAnEmptyMachine(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.commit(t)
	if !config.Migrated(m.ConfigDir) || Needed(m.ConfigDir, m.StateDir) {
		t.Fatal("an empty machine was not marked migrated")
	}
}

// A profile signed in with its team's server key stays single-team: its key moves under
// its team's enrollment, with no sign-in to move.
func TestRunMigratesAServerKeyProfile(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	ci := legacy{profile: "ci", org: "acme", team: "acme-platform", entries: []string{"github.com/acme/api"}, serverKey: true, key: "ter_srv_ci"}
	m.setUp(t, ci)
	m.commit(t)
	m.migrated(t, ci)
	file, err := config.LoadFile(m.ConfigDir)
	must(t, err)
	if !file.Profiles["ci"].ServerKeySignIn {
		t.Fatal("the migration forgot the profile signs in with its server key")
	}
}

// A legacy team in global mode collects nothing once migrated: only a personal
// organization may collect everything, and a migrated profile has no personal team.
func TestRunMigratesAGlobalTeamToCollectNothing(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	global := legacy{profile: "default", org: "acme", team: "acme-all", global: true, key: "ter_srv_all"}
	m.setUp(t, global)
	m.commit(t)
	id := m.migrated(t, global)
	s := m.state(t, "default")
	if tm := s.Teams[0]; tm.Policy.Global() || len(tm.Policy.Repositories) != 0 || len(s.Bindings) != 0 {
		t.Fatalf("the global team became %+v, bindings %v", tm, s.Bindings)
	}
	r := routing.NewRouter(s)
	for _, loc := range []routing.Location{{Kind: routing.OutsideGit}, {Kind: routing.Repository, Repository: "github.com/acme/api"}, {Kind: routing.Unknown}} {
		if d := r.Route(loc, time.Now()); d.Outcome == routing.Send {
			t.Fatalf("%+v goes to %s", loc, d.EnrollmentID)
		}
	}
	if v, _ := r.Recheck(routing.Destination{EnrollmentID: id, Location: routing.Location{Kind: routing.OutsideGit}}, time.Now()); v == routing.SendNow {
		t.Fatal("a legacy record outside git is still sent")
	}
}

// A team no policy was ever validated for stays selected under its enrollment, with its
// key, and blocks nothing until the relay's refresh validates its policy.
func TestRunKeepsATeamWithNoValidatedPolicy(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	fresh := legacy{profile: "default", org: "acme", team: "acme-platform", noPolicy: true, key: "ter_srv_acme"}
	m.setUp(t, fresh)
	m.commit(t)
	m.migrated(t, fresh)
	tm := m.state(t, "default").Teams[0]
	if tm.StateAt(time.Now()) != routing.Expired || len(tm.Policy.Repositories) != 0 {
		t.Fatalf("a team with no validated policy migrated as %+v", tm)
	}
	pol := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/api"}, TeamID: tm.TeamID,
		OrganizationID: tm.OrganizationID, AuthURL: tm.AuthURL, Revision: 1, FetchedAt: time.Now()}
	_, err := routing.Update(m.StateDir, "default", func(s *routing.State) error {
		_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &pol})
		return err
	})
	must(t, err)
	if got := m.state(t, "default").Teams[0].StateAt(time.Now()); got != routing.Active {
		t.Fatalf("after its first refresh the team is %s", got)
	}
}

// A policy cached for another organization is not the team's grant, as today: the team
// migrates with none, and nothing is bound to the other organization.
func TestRunIgnoresAPolicyOfAnotherOrganization(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.setUp(t, acme)
	stale := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/other/x"}, TeamID: acme.team,
		OrganizationID: "other", AuthURL: m.authURL(t, "default"), Revision: 1, FetchedAt: time.Now()}
	must(t, config.WritePolicy(m.StateDir, stale))
	m.commit(t)
	s := m.state(t, "default")
	if tm := s.Teams[0]; tm.OrganizationID != "acme" || len(tm.Policy.Repositories) != 0 || len(s.Bindings) != 0 {
		t.Fatalf("migrated %+v with bindings %v", tm, s.Bindings)
	}
}

// Only the profile's own team maps to its enrollment. A key kept for another project, such
// as a team selected before, is retired with keys.json and never copied: records queued
// for it are dropped.
func TestRunMapsOnlyTheProfilesOwnLegacyProject(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	// An agent's own key for the team, then the team's key setup kept in its place.
	must(t, keystore.SetFor(m.ConfigDir, "claude", acme.team, "ter_srv_harness", keystore.Hosts{}))
	m.setUp(t, acme)
	must(t, keystore.Set(m.ConfigDir, "acme-old-team", "ter_srv_old", keystore.Hosts{}))
	m.commit(t)
	id := m.migrated(t, acme)
	if legacy := m.state(t, "default").Migration.Legacy; legacy["acme-old-team"] != "" {
		t.Fatalf("another project maps to an enrollment: %v", legacy)
	}
	b, err := os.ReadFile(config.KeysV2Path(m.ConfigDir))
	must(t, err)
	if strings.Contains(string(b), "acme-old-team") || strings.Count(string(b), `"project"`) != 1 {
		t.Fatalf("keys.v2.json holds more than the profile's team:\n%s", b)
	}
	if key, _, _ := keystore.GetTeam(m.ConfigDir, id); key != acme.key {
		t.Fatalf("the enrollment's key is %q, not the team's own", key)
	}
	if got, _ := keystore.GetFor(m.ConfigDir, "claude", acme.team); got != "" {
		t.Fatal("an agent's own key survived in keys.json")
	}
}

// Two profiles on the same team each get their own enrollment and their own copy of the
// team's one key.
func TestRunGivesTwoProfilesOfOneTeamTheirOwnEnrollments(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.setUp(t, acme)
	twin := acme
	twin.profile = "twin"
	m.setUp(t, twin)
	m.commit(t)
	a, b := m.migrated(t, acme), m.migrated(t, twin)
	if a == b {
		t.Fatal("two profiles share one enrollment")
	}
}

// A routing state this migration did not prepare is left as it is.
func TestRunLeavesAStateItDidNotPrepare(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.setUp(t, acme)
	m.setUp(t, legacy{profile: "fresh", org: "me"})
	_, err := routing.Create(m.StateDir, "fresh", func(*routing.State) error { return nil })
	must(t, err)
	m.commit(t)
	m.migrated(t, acme)
	if s := m.state(t, "fresh"); s.Migration != nil || s.Generation != 1 {
		t.Fatalf("the migration rewrote a state it did not prepare: %+v", s)
	}
	if !slices.Equal(sortedProfiles(t, m), []string{"default", "fresh"}) {
		t.Fatalf("routing states: %v", sortedProfiles(t, m))
	}
}

func sortedProfiles(t *testing.T, m Machine) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.StateDir, routing.StatesDir))
	if !errors.Is(err, fs.ErrNotExist) {
		must(t, err)
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
