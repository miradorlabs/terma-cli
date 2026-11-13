package hookrun

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// mixed reports whether sessionID's current run is Mixed, in the ledger and in its claim.
func (m *routedMachine) mixed(sessionID string) (ledger, placement bool) {
	_, ledger = runs.MixedAt(m.stateDir, sessionID, time.Time{})
	c, ok := claim.Read(m.stateDir, sessionID, time.Now())
	return ledger, ok && c.Kind == claim.KindMixed
}

// The plan's scenario: a session started in github.com/dawson/side, personal's, edits
// github.com/acme/api. From that edit's hook the run is Mixed, in the run ledger and in a
// mixed placement, while the edit's file event goes to Acme Platform by its own checkout.
func TestATouchOfAnotherOrganizationsCheckoutMakesTheRunMixed(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonal(t)
	m := newRoutedMachine(t, st)
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "crossing-session"
	r := startIn(t, m.hook(), sid, side)
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatal("Mixed before any crossing")
	}
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, nil)
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("after the crossing: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	touches := hookruntest.Named(m.spooled(), semconv.TermaFilesTouchedEvent)
	if len(touches) != 1 || touches[0].Enrollment != hookruntest.Enrollment(t, st, "acme-platform") || !touches[0].OwnLocation ||
		touches[0].Repository.Origin != "github.com/acme/api" {
		t.Fatalf("file events %+v, want one to Acme Platform by its own checkout", touches)
	}
}

// Every touched checkout is classified before anything filters it: one under a binding
// still to decide crosses though nothing collects it yet, and one nothing collects and
// nothing binds belongs to no organization and does not.
func TestEveryTouchedCheckoutIsClassifiedBeforeFiltering(t *testing.T) {
	t.Parallel()
	// ClientCo lists github.com/torvalds/linux, which nobody has decided; personal collects
	// only github.com/dawson/*, so github.com/other/lib is nobody's.
	st := hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("clientco", "clientco-contractors", true, "github.com/torvalds/linux"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, nil)
	for name, tc := range map[string]struct {
		origin string
		mixed  bool
	}{
		"pending":  {"github.com/torvalds/linux", true},
		"nobody's": {"github.com/other/lib", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, st)
			side, other := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, tc.origin)
			r := startIn(t, m.hook(), "touching-session", side)
			m.hook().Touch(r, sessionOf("touching-session"), "Read", []string{filepath.Join(other, "README")}, nil)
			if ledger, placement := m.mixed("touching-session"); ledger != tc.mixed || placement != tc.mixed {
				t.Errorf("Mixed in the ledger %v, in the placement %v; want %v", ledger, placement, tc.mixed)
			}
			for _, ev := range hookruntest.Named(m.spooled(), semconv.TermaFilesTouchedEvent) {
				if ev.Repository.Origin == "github.com/other/lib" {
					t.Errorf("spooled a file event for a checkout nothing collects: %+v", ev)
				}
			}
		})
	}
}

// A subagent's crossing is its parent's: the session's run turns Mixed, and so does the
// subagent's own placement.
func TestASubagentsCrossingMakesItsParentsRunMixed(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid, agent = "parent-session", "agent-9"
	r := startIn(t, m.hook(), sid, side)
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, AgentAttrs(map[string]any{}, agent, "general-purpose"))
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("the parent's run: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	if c := m.claim(agent); c.Kind != claim.KindMixed {
		t.Errorf("the subagent's placement %+v", c)
	}
	if _, mixed := runs.MixedAt(m.stateDir, agent, time.Now()); !mixed {
		t.Error("the subagent's ID does not read its session's Mixed run")
	}
}

// The run's own placement moving counts as touching: a company run whose directory moves
// outside git, where personal collects, has crossed.
func TestACompanyRunMovingOutsideGitCrosses(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	api := hookruntest.RepoAt(t, "github.com/acme/api")
	scratch, _ := filepath.EvalSymlinks(t.TempDir())
	const sid = "moving-session"
	startIn(t, m.hook(), sid, api)
	ClaimFromPayload(context.Background(), m.hook(), PayloadSession{ID: sid, Cwd: scratch}, "claude-code")
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
	}
}

// Mixed only turns on: a later hook of the run, back in its first repository, leaves the
// mixed placement as it is. Only a new run, a resume, ends it.
func TestOnlyANewRunEndsMixed(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "resumed-session"
	r := startIn(t, m.hook(), sid, side)
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, nil)
	env := m.hook()
	r, _ = env.Open(context.Background(), sid, side)
	env.EmitFor(r, spool.Event{Name: semconv.TermaToolCallEvent, SessionID: sid, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "claude-code"}})
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("a later hook of the run turned Mixed off: ledger %v, placement %v", ledger, placement)
	}
	startIn(t, m.hook(), sid, side)
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatalf("the resumed run is still Mixed: ledger %v, placement %v", ledger, placement)
	}
	if c := m.claim(sid); c.Kind != claim.KindRepository || c.Repository.Origin != "github.com/dawson/side" {
		t.Errorf("the new run's placement %+v", c)
	}
}
