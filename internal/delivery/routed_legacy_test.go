package delivery

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// legacyEvent is an event queued before the migration: it names only its project.
func legacyEvent(project, origin, mark string) spool.Event {
	e := spool.Event{Time: time.Now(), Name: semconv.TermaCommitEvent, Repository: config.Repository{Origin: origin},
		Attrs: map[string]any{semconv.TermaTurnIDKey: mark}}
	if project != "" {
		e.Attrs[hookrun.AttrProjectID] = project
	}
	return e
}

// migratedFrom creates profile's committed routing state from its single team, whose
// legacy project maps to its cutover enrollment, and returns that enrollment.
func (m *machine) migratedFrom(profile string, team routing.Team, binding string) string {
	m.t.Helper()
	if err := config.UpdateProfile(m.config, profile, func(*config.Profile) {}); err != nil {
		m.t.Fatal(err)
	}
	return m.route(profile, []routing.Team{team}, func(s *routing.State, e map[string]string) error {
		s.Migration = &routing.Migration{Phase: routing.Committed, Legacy: map[string]string{team.TeamID: e[team.TeamID]}}
		return s.Bind(binding, team.OrganizationID, false)
	})[team.TeamID]
}

// An event queued before the migration goes to the cutover enrollment of the profile whose
// team it names, whichever profile that is, and only while that enrollment lasts; one
// naming any other project is dropped, and one naming none is unroutable, as before.
func TestEventsQueuedBeforeTheMigrationGoToTheirCutoverEnrollment(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	acmeHost, globexHost := newIngest(t), newIngest(t)
	acme := m.migratedFrom(config.DefaultProfile, orgTeam("acme", "acme-platform", "github.com/acme/app"), "github.com/acme/app")
	globex := m.migratedFrom("work", orgTeam("globex", "globex-team", "github.com/globex/app"), "github.com/globex/app")
	m.key(acme, "acme-platform", "ter_srv_acme", acmeHost)
	m.key(globex, "globex-team", "ter_srv_globex", globexHost)
	m.queue(
		legacyEvent("acme-platform", "github.com/acme/app", "acme-commit"),
		legacyEvent("globex-team", "github.com/globex/app", "globex-commit"),
		legacyEvent("selected-earlier", "github.com/acme/app", "earlier-commit"),
		legacyEvent("", "github.com/acme/app", "no-team-commit"),
	)
	res := m.flush(m.router())
	if res.Sent != 2 || res.Discarded != 1 || res.Unroutable != 1 || m.pending() != 0 {
		t.Fatalf("Flush = %+v, pending %d", res, m.pending())
	}
	for host, want := range map[*ingest][]string{acmeHost: {"Bearer ter_srv_acme", "acme-commit"}, globexHost: {"Bearer ter_srv_globex", "globex-commit"}} {
		keys, body := host.sent()
		if !slices.Equal(keys, want[:1]) || !strings.Contains(body, want[1]) || strings.Contains(body, hookrun.AttrProjectID) || strings.Contains(body, "earlier-commit") {
			t.Errorf("%s was sent %v:\n%s", want[1], keys, body)
		}
	}

	// The cutover enrollment ends: its legacy events are dropped, the team ticked again
	// or not.
	m.update(config.DefaultProfile, func(s *routing.State) error {
		s.Deselect(acme)
		again, err := s.Select(orgTeam("acme", "acme-platform", "github.com/acme/app"))
		m.key(again, "acme-platform", "ter_srv_acme_again", acmeHost)
		return err
	})
	m.queue(legacyEvent("acme-platform", "github.com/acme/app", "after-unticking"))
	if res := m.flush(m.router()); res.Sent != 0 || res.Discarded != 1 {
		t.Fatalf("after its enrollment ended: %+v", res)
	}
	if _, body := acmeHost.sent(); strings.Contains(body, "after-unticking") {
		t.Fatal("a legacy event left under a later enrollment")
	}
}

// A legacy event held for its team to recover keeps the destination it was mapped to.
func TestALegacyEventKeepsItsMappedDestination(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	acme := m.migratedFrom(config.DefaultProfile, orgTeam("acme", "acme-platform", "github.com/acme/app"), "github.com/acme/app")
	m.update(config.DefaultProfile, func(s *routing.State) error { s.Teams[0].Refusals = 1; return nil })
	m.queue(legacyEvent("acme-platform", "github.com/acme/app", "held"))
	if res := m.flush(m.router()); res.Blocked != 1 {
		t.Fatalf("Flush = %+v", res)
	}
	queued, err := m.spool.Peek(10)
	if err != nil || len(queued) != 1 || queued[0].Profile != config.DefaultProfile || queued[0].Enrollment != acme {
		t.Fatalf("queued %+v, %v", queued, err)
	}
}

// A migrated machine sends nothing for a profile whose routing state is missing,
// unreadable, or still being prepared by the migration: its events, routed or queued
// before the migration, stay in the spool until the state can be read.
func TestAProfileWithoutAUsableRoutingStateSendsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state func(m *machine, team routing.Team) string
		fix   func(m *machine)
	}{
		{"missing", func(*machine, routing.Team) string { return "enrollment-of-a-lost-state" }, nil},
		{"unreadable", func(m *machine, _ routing.Team) string {
			path, _ := routing.StatePath(m.state, config.DefaultProfile)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				m.t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				m.t.Fatal(err)
			}
			return "enrollment-of-an-unreadable-state"
		}, nil},
		{"preparing", func(m *machine, team routing.Team) string {
			e := m.migratedFrom(config.DefaultProfile, team, "github.com/acme/app")
			m.update(config.DefaultProfile, func(s *routing.State) error { s.Migration.Phase = routing.Preparing; return nil })
			return e
		}, func(m *machine) {
			m.update(config.DefaultProfile, func(s *routing.State) error { s.Migration.Phase = routing.Committed; return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := migratedMachine(t)
			host := newIngest(t)
			team := orgTeam("acme", "acme-platform", "github.com/acme/app")
			enrollment := tc.state(m, team)
			m.key(enrollment, "acme-platform", "ter_srv_acme", host)
			m.queue(routedEvent(enrollment, "github.com/acme/app", "s1", "routed"), legacyEvent("acme-platform", "github.com/acme/app", "legacy"))
			res := m.flush(m.router())
			if keys, _ := host.sent(); len(keys) != 0 || res.Sent != 0 || res.Unrouted != 2 || res.Held != 2 || m.pending() != 2 {
				t.Fatalf("Flush = %+v, sent %v, pending %d", res, keys, m.pending())
			}
			if tc.fix == nil {
				return
			}
			tc.fix(m)
			if res := m.flush(m.router()); res.Sent != 2 || m.pending() != 0 {
				t.Fatalf("once committed: %+v, pending %d", res, m.pending())
			}
		})
	}
}

// A profile's Router is compiled once per state generation and reused until it moves.
func TestARouterIsReusedWhileItsStateIsUnchanged(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	m.migratedFrom(config.DefaultProfile, orgTeam("acme", "acme-platform", "github.com/acme/app"), "github.com/acme/app")
	c := routeCache{}
	first := c.load(m.state, config.DefaultProfile)
	if first == nil || c.load(m.state, config.DefaultProfile) != first {
		t.Fatal("an unchanged state was compiled again")
	}
	m.update(config.DefaultProfile, func(s *routing.State) error { return s.Refuse("github.com/dawson") })
	next := c.load(m.state, config.DefaultProfile)
	if next == nil || next == first || next.state.Generation <= first.state.Generation {
		t.Fatal("a new generation kept the old Router")
	}
	m.update(config.DefaultProfile, func(s *routing.State) error { s.Migration.Phase = routing.Preparing; return nil })
	if c.load(m.state, config.DefaultProfile) != nil || c[config.DefaultProfile] != nil {
		t.Fatal("a state being prepared kept a Router")
	}
}

// A machine not yet migrated delivers as it always has, whatever else is on disk: by each
// event's attrs.project_id with the legacy key, its destination fields and any routing
// state or team key ignored.
func TestAnUnmigratedMachineDeliversAsToday(t *testing.T) {
	t.Parallel()
	dir, stateDir := t.TempDir(), t.TempDir()
	legacyHost, teamHost := newIngest(t), newIngest(t)
	if err := keystore.Set(dir, "acme-platform", "ter_srv_legacy", keystore.Hosts{OTLP: legacyHost.URL}); err != nil {
		t.Fatal(err)
	}
	m := &machine{t: t, config: dir, state: stateDir}
	enrollment := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/app")}, nil)["acme-platform"]
	m.key(enrollment, "acme-platform", "ter_srv_team", teamHost)
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	both := legacyEvent("acme-platform", "github.com/acme/app", "both")
	both.Profile, both.Enrollment = config.DefaultProfile, enrollment
	only := routedEvent(enrollment, "github.com/acme/app", "s1", "only")
	if err := s.Append(both, only); err != nil {
		t.Fatal(err)
	}
	pol := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/app"}, TeamID: "acme-platform", FetchedAt: time.Now()}
	r := Router{ConfigDir: dir, StateDir: stateDir, Policy: func(context.Context, *config.Config, string) (config.Policy, error) { return pol, nil }}
	res := r.Flush(t.Context(), s, &config.Config{}, true, 0)
	if res.Migrated || res.Sent != 1 || res.Unroutable != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	if keys, _ := legacyHost.sent(); !slices.Equal(keys, []string{"Bearer ter_srv_legacy"}) {
		t.Fatalf("the legacy host was sent %v", keys)
	}
	if keys, _ := teamHost.sent(); len(keys) != 0 {
		t.Fatalf("the team key's host was sent %v before migration", keys)
	}
}
