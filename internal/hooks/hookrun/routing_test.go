package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// routedMachine is a migrated machine's hooks: a state directory and spool they share, and
// the routing state st each hook, its own process, loads afresh.
type routedMachine struct {
	t        *testing.T
	stateDir string
	sp       *spool.Spool
	st       *routing.State
}

func newRoutedMachine(t *testing.T, st *routing.State) *routedMachine {
	t.Helper()
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &routedMachine{t: t, stateDir: t.TempDir(), sp: sp, st: st}
}

// hook is one hook's environment, routing as a hook process does: one Routing of its own.
func (m *routedMachine) hook() Env {
	return Env{StateDir: m.stateDir, ConfigDir: m.t.TempDir(), Now: time.Now(), Spool: m.sp, Policy: config.NoPolicy("", ""),
		Routing: NewRouting(hookruntest.RoutedProfile, m.st)}
}

func (m *routedMachine) spooled() []spool.Event { return hookruntest.Spooled(m.t, m.sp) }

func (m *routedMachine) claim(sessionID string) claim.Claim {
	m.t.Helper()
	c, ok := claim.Read(m.stateDir, sessionID, time.Now())
	if !ok {
		m.t.Fatalf("no claim for %s", sessionID)
	}
	return c
}

// acmeAndPersonal is the plan's two-organization machine: Acme Platform collecting
// github.com/acme/*, with github.com/acme bound to Acme, then personal collecting the rest.
func acmeAndPersonal(t *testing.T) *routing.State { return hookruntest.TwoOrganizations(t) }

// startIn is a session start in root: a new run, the session announced, as an agent's
// SessionStart hook does.
func startIn(t *testing.T, env Env, sessionID, root string) *Repo {
	t.Helper()
	env.StartRun(sessionID)
	r, ok := env.Open(context.Background(), sessionID, root)
	if !ok {
		t.Fatalf("%s: routing collects nothing there", root)
	}
	env.Announce(r, env.NewSession(r, sessionID, "claude-code", "m"), nil)
	return r
}

// On a migrated machine every spooled event records the destination Route gave it, its
// run and whether its location is its own, and never a project; the session's placement
// names its location alone, in the run it began.
func TestARoutedSessionSpoolsItsDestinationNotAProject(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonal(t)
	m := newRoutedMachine(t, st)
	root := hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "routed-session"
	startIn(t, m.hook(), sid, root)
	env := m.hook()
	r, _ := env.Open(context.Background(), sid, root)
	env.Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(root, "main.go")}, nil)

	events := m.spooled()
	acme := hookruntest.Enrollment(t, st, "acme-platform")
	if len(events) != 2 {
		t.Fatalf("spooled %s", hookruntest.Names(events))
	}
	for _, ev := range events {
		if ev.Profile != hookruntest.RoutedProfile || ev.Enrollment != acme || ev.HoldKey != "" || ev.Global || ev.Repository.Origin != "github.com/acme/api" {
			t.Errorf("%s: destination %q %q %q, repository %+v", ev.Name, ev.Profile, ev.Enrollment, ev.HoldKey, ev.Repository)
		}
		if _, ok := ev.Attrs[AttrProjectID]; ok {
			t.Errorf("%s names a project: %v", ev.Name, ev.Attrs)
		}
	}
	start, touch := hookruntest.Named(events, semconv.TermaSessionStartEvent)[0], hookruntest.Named(events, semconv.TermaFilesTouchedEvent)[0]
	if start.Run == "" || start.OwnLocation || touch.Run != start.Run || !touch.OwnLocation {
		t.Errorf("start run %q own %v; touch run %q own %v: the start is its run's, the file event its checkout's", start.Run, start.OwnLocation, touch.Run, touch.OwnLocation)
	}
	c := m.claim(sid)
	if c.Kind != claim.KindRepository || c.ProjectID != "" || c.Run != start.Run || c.Repository.Origin != "github.com/acme/api" ||
		c.Profile != hookruntest.RoutedProfile {
		t.Errorf("placement %+v: location-only, in the session's run, under the hook's profile", c)
	}
	if manifests, _ := r.Store.Manifests(); len(manifests) != 1 {
		t.Errorf("manifests %+v: a location Route sends records its touched files", manifests)
	}
	if _, mixed := runs.MixedAt(m.stateDir, sid, time.Now()); mixed {
		t.Error("a run in one organization is not Mixed")
	}
}

// Where Route holds a location (a binding still to decide, or a team that cannot send now)
// a hook records and spools as where it sends, each event naming the team holding it and,
// for a pending binding, that binding.
func TestARoutedHoldRecordsAndSpoolsWithWhatHoldsIt(t *testing.T) {
	t.Parallel()
	expired := hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*")
	expired.Policy.FetchedAt = time.Now().Add(-config.MaxPolicyAge - time.Hour)
	for name, tc := range map[string]struct {
		st      *routing.State
		holdKey string
	}{
		"pending": {hookruntest.RoutingState(t, []routing.Team{hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"), hookruntest.RoutedPersonal()}, nil), "github.com/acme"},
		"expired": {hookruntest.RoutingState(t, []routing.Team{expired, hookruntest.RoutedPersonal()}, map[string]string{"github.com/acme": "acme"}), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, tc.st)
			root := hookruntest.RepoAt(t, "github.com/acme/api")
			r := startIn(t, m.hook(), "held-session", root)
			m.hook().Touch(r, sessionOf("held-session"), "Edit", []string{filepath.Join(root, "main.go")}, nil)
			if manifests, _ := r.Store.Manifests(); len(manifests) != 1 {
				t.Errorf("manifests %+v", manifests)
			}
			events := m.spooled()
			if len(events) != 2 {
				t.Fatalf("spooled %s", hookruntest.Names(events))
			}
			for _, ev := range events {
				if ev.Enrollment != hookruntest.Enrollment(t, tc.st, "acme-platform") || ev.HoldKey != tc.holdKey {
					t.Errorf("%s: %q %q, want held for Acme Platform by %q", ev.Name, ev.Enrollment, ev.HoldKey, tc.holdKey)
				}
			}
		})
	}
}

// Where Route drops a location a hook writes no manifest and spools nothing, as today in a
// repository nobody collects, but still places the session there, by location alone: never
// a claim.Mark, which would read as "not collected" everywhere.
func TestARoutedDropOnlyPlacesTheSession(t *testing.T) {
	t.Parallel()
	st := hookruntest.RoutingState(t, []routing.Team{hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*")},
		map[string]string{"github.com/acme": "acme"})
	m := newRoutedMachine(t, st)
	root := hookruntest.RepoAt(t, "github.com/other/side")
	const sid = "dropped-session"
	env := m.hook()
	env.StartRun(sid)
	if _, ok := env.Open(context.Background(), sid, root); ok {
		t.Fatal("a location Route drops resolved for recording")
	}
	claimed := false
	env.OnClaim = func(string) { claimed = true }
	if ClaimFromPayload(context.Background(), env, PayloadSession{ID: sid, Cwd: root}, "claude-code") || claimed {
		t.Error("a dropped location starts the relay and the commit hooks")
	}
	c := m.claim(sid)
	if c.Kind != claim.KindRepository || c.ProjectID != "" || c.Repository.Origin != "github.com/other/side" || c.Run == "" {
		t.Errorf("placement %+v, want the location alone, in the session's run", c)
	}
	if events := m.spooled(); len(events) != 0 {
		t.Errorf("spooled %s", hookruntest.Names(events))
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "terma")); !os.IsNotExist(err) {
		t.Errorf("a session store was written: %v", err)
	}
}

// Outside git a hook places the session by its directory, and personal, collecting
// everything, takes what it records there.
func TestARoutedSessionOutsideGitIsPersonals(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonal(t)
	m := newRoutedMachine(t, st)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	const sid = "scratch-session"
	if !ClaimFromPayload(context.Background(), m.hook(), PayloadSession{ID: sid, Cwd: dir}, "claude-code") {
		t.Fatal("personal collects outside git, but the payload's claim was not reported")
	}
	if c := m.claim(sid); c.Kind != claim.KindOutsideGit || c.ProjectID != "" || c.Cwd != dir || c.Repository != (config.Repository{}) {
		t.Errorf("placement %+v, want outside git at %s", c, dir)
	}
	startIn(t, m.hook(), sid, dir)
	events := m.spooled()
	if len(events) != 1 || events[0].Enrollment != hookruntest.Enrollment(t, st, "personal") || events[0].Repository != (config.Repository{}) {
		t.Fatalf("spooled %+v, want personal's, with no repository", events)
	}
}

// A migrated machine whose routing state is missing, unreadable or still being prepared by
// the migration routes nothing: hooks place no session and spool nothing.
func TestARoutedMachineWithoutAStateSendsNothing(t *testing.T) {
	t.Parallel()
	preparing := acmeAndPersonal(t)
	preparing.Migration = &routing.Migration{Phase: routing.Preparing}
	for name, st := range map[string]*routing.State{"missing": nil, "preparing": preparing} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, st)
			root := hookruntest.RepoAt(t, "github.com/acme/api")
			env := m.hook()
			env.StartRun("lost-session")
			if _, ok := env.Open(context.Background(), "lost-session", root); ok {
				t.Error("resolved for recording")
			}
			if ClaimFromPayload(context.Background(), env, PayloadSession{ID: "lost-session", Cwd: root}, "claude-code") {
				t.Error("claimed")
			}
			if _, ok := claim.Read(m.stateDir, "lost-session", time.Now()); ok {
				t.Error("placed the session")
			}
			if _, err := os.Stat(filepath.Join(m.stateDir, runs.Dir)); !os.IsNotExist(err) {
				t.Errorf("started a run: %v", err)
			}
			if events := m.spooled(); len(events) != 0 {
				t.Errorf("spooled %s", hookruntest.Names(events))
			}
		})
	}
}

// A subagent whose telemetry names its own ID is placed under it too, and is an alias of
// its session in the run ledger, so the session's run turning Mixed covers it.
func TestARoutedSubagentIsAnAliasOfItsSession(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	root := hookruntest.RepoAt(t, "github.com/acme/api")
	const sid, agent = "parent-session", "agent-7"
	r := startIn(t, m.hook(), sid, root)
	m.hook().EmitFor(r, spool.Event{Name: semconv.TermaSubagentStartEvent, SessionID: sid, Attrs: AgentAttrs(map[string]any{semconv.GenAIMainAgentNameKey: "claude-code"}, agent, "Explore")})
	if c := m.claim(agent); c.Kind != claim.KindRepository || c.ProjectID != "" || c.Run != m.claim(sid).Run {
		t.Errorf("subagent placement %+v", c)
	}
	if err := runs.MarkMixed(m.stateDir, sid, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, mixed := runs.MixedAt(m.stateDir, agent, time.Now()); !mixed {
		t.Error("the subagent's ID does not share its session's run")
	}
}

func sessionOf(id string) session.Session {
	return session.Session{ID: id, Tool: "claude-code", Model: "m"}
}
