package codex

import (
	"context"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// On a migrated machine what a rollout turn recorded routes by the directory the turn
// names, its own location, rather than by where the hook reading it runs: a thread that
// ran its first turn in a personal repository and was resumed in Acme's sends each turn's
// replies and quota to its own team. Its name, which any turn may have given it, is the
// thread's and goes with the session's run. Nothing names a project.
func TestRoutedCodexTurnsGoWhereTheyRan(t *testing.T) {
	st := hookruntest.TwoOrganizations(t)
	env := fundingEnv(t)
	routeCodex(t, &env)
	env.Policy, env.Team, env.Routing = config.NoPolicy("", ""), "", hookrun.NewRouting(hookruntest.RoutedProfile, st)
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	evs := stopResumed(t, env, side, env.Cwd)

	personal, acme := hookruntest.Enrollment(t, st, "personal"), hookruntest.Enrollment(t, st, "acme-platform")
	byTurn := map[any]string{resumedTurn1: personal, resumedTurn2: acme}
	counted := map[string]int{}
	for _, e := range evs {
		if _, ok := e.Attrs[hookrun.AttrProjectID]; ok || e.Profile != hookruntest.RoutedProfile {
			t.Errorf("%s: profile %q, attrs %v", e.Name, e.Profile, e.Attrs)
		}
		switch e.Name {
		case semconv.TermaAssistantMessageEvent, semconv.TermaSessionQuotaEvent:
			want, ok := byTurn[e.Attrs[semconv.TermaTurnIDKey]]
			if !ok {
				continue
			}
			if e.Enrollment != want || !e.OwnLocation {
				t.Errorf("%s of turn %v: enrollment %q own %v, want %q by its own directory", e.Name, e.Attrs[semconv.TermaTurnIDKey], e.Enrollment, e.OwnLocation, want)
			}
			counted[e.Enrollment]++
		case semconv.TermaSessionTitleEvent:
			if e.Enrollment != acme || e.OwnLocation {
				t.Errorf("title: enrollment %q own %v, want the session's run's", e.Enrollment, e.OwnLocation)
			}
		}
	}
	if counted[personal] == 0 || counted[acme] == 0 {
		t.Fatalf("turn events by destination %v: each turn sends its own", counted)
	}
	if got := len(hookruntest.Named(evs, semconv.TermaSessionTitleEvent)); got != 1 {
		t.Errorf("%d titles, want the thread's one", got)
	}
}

// The hosted actions a turn ran route by that turn's directory too.
func TestRoutedCodexDesktopActivityGoesWhereItsTurnRan(t *testing.T) {
	st := hookruntest.TwoOrganizations(t)
	env := fundingEnv(t)
	routeCodex(t, &env)
	env.Policy, env.Team, env.Routing = config.NoPolicy("", ""), "", hookrun.NewRouting(hookruntest.RoutedProfile, st)
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	path := sequenceFixture(t, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`+"\n"+turnContext("turn-1", side)+
		`{"timestamp":"2026-10-03T22:01:40Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-1","item":{"type":"Extension","id":"item_1","kind":"web.search"}}}`+"\n")
	r, ok := env.Open(context.Background(), testCodexID, env.Cwd)
	if !ok {
		t.Fatal("the hook's repository is not collected")
	}
	captureCodexDesktopActivity(context.Background(), env, r, &codexHookInput{SessionID: testCodexID, TranscriptPath: path})
	evs := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaToolCallEvent)
	if len(evs) != 1 || evs[0].Enrollment != hookruntest.Enrollment(t, st, "personal") || !evs[0].OwnLocation || evs[0].Attrs[hookrun.AttrProjectID] != nil {
		t.Fatalf("hosted actions %+v, want one to personal by its turn's directory", evs)
	}
}
