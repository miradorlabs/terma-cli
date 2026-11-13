package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// On a migrated machine a commit is stamped only where Route sends its repository to a team
// whose policy turns git_hooks on, which is also where the commit hooks are installed: a
// team that turns git_hooks off stops trailers where the hooks already are, and a team that
// cannot send, or a repository nobody collects any more, gets none.
func TestARoutedCommitIsStampedOnlyWhereRouteSendsWithGitHooks(t *testing.T) {
	t.Parallel()
	team := func(gitHooks bool, fetched time.Time) *routing.State {
		acme := hookruntest.RoutedTeam("acme", "acme-platform", gitHooks, "github.com/acme/*")
		acme.Policy.FetchedAt = fetched
		return hookruntest.RoutingState(t, []routing.Team{acme}, map[string]string{"github.com/acme": "acme"})
	}
	recent := time.Now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		st      *routing.State
		stamped bool
	}{
		"sent with git_hooks on":   {team(true, recent), true},
		"git_hooks off":            {team(false, recent), false},
		"held, the policy expired": {team(true, time.Now().Add(-config.MaxPolicyAge-time.Hour)), false},
		"collected by nobody":      {hookruntest.RoutingState(t, nil, nil), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The session edited the file while Acme collected the repository with git_hooks on.
			m := newRoutedMachine(t, team(true, recent))
			root := hookruntest.RepoAt(t, "github.com/acme/api")
			hookruntest.WriteFile(t, root, "main.go", "package main\n")
			const sid = "committing-session"
			r := startIn(t, m.hook(), sid, root)
			m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(root, "main.go")}, nil)
			m.spooled()

			m.st = tc.st
			env := func(args ...string) Env { e := m.hook(); e.Cwd, e.Args = root, args; return e }
			if got := env().Routing.GitHooks(filepath.Join(root, ".git"), time.Now()); got != tc.stamped {
				t.Errorf("GitHooks = %v, want %v: hooks go where commits are stamped", got, tc.stamped)
			}
			commitAs(t, env, root, "feat: x", "main.go")
			msg, _ := gitx.Git(context.Background(), root, "log", "-1", "--format=%B")
			stamped := len(trailer.Parse(msg, "")) > 0
			if stamped != tc.stamped {
				t.Fatalf("stamped %v, want %v: %q", stamped, tc.stamped, msg)
			}
			events := m.spooled()
			if got := len(hookruntest.Named(events, semconv.TermaCommitStampedEvent)); got != map[bool]int{true: 1}[tc.stamped] {
				t.Errorf("%d commit stamps spooled", got)
			}
			for _, ev := range events {
				if ev.Enrollment == "" || !ev.OwnLocation || ev.Run != "" || ev.Attrs[AttrProjectID] != nil {
					t.Errorf("%s: enrollment %q, own %v, run %q, attrs %v: a commit's location is its repository's own",
						ev.Name, ev.Enrollment, ev.OwnLocation, ev.Run, ev.Attrs)
				}
			}
		})
	}
}

// A push is recorded with the destination its repository's route gives it, before the
// detached report spools it: the report never routes it again.
func TestARoutedPushRecordsItsDestination(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonal(t)
	m := newRoutedMachine(t, st)
	root := hookruntest.RepoAt(t, "github.com/acme/api")
	hookruntest.WriteFile(t, root, "main.go", "package main\n")
	ctx := context.Background()
	for _, args := range [][]string{{"add", "main.go"}, {"commit", "-q", "-m", "init"}} {
		if _, err := gitx.Git(ctx, root, args...); err != nil {
			t.Fatal(err)
		}
	}
	sha, _ := gitx.Git(ctx, root, "rev-parse", "HEAD")
	var recorded string
	env := m.hook()
	env.Cwd, env.Args, env.AwaitPush = root, []string{"origin", "https://github.com/acme/api.git"}, func(path string) { recorded = path }
	env.Stdin = strings.NewReader("refs/heads/main " + strings.TrimSpace(sha) + " refs/heads/main " + strings.Repeat("0", 40) + "\n")
	if err := PrePush(ctx, env); err != nil || recorded == "" {
		t.Fatalf("PrePush: %v, recorded %q", err, recorded)
	}
	data, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	var rec pushRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	ev := rec.Event
	if ev.Profile != hookruntest.RoutedProfile || ev.Enrollment != hookruntest.Enrollment(t, st, "acme-platform") || !ev.OwnLocation || ev.Run != "" ||
		ev.Attrs[AttrProjectID] != nil || ev.Repository.Origin != "github.com/acme/api" {
		t.Fatalf("recorded push event %+v", ev)
	}
}

// An event a sandboxed hook parks carries its destination, which the hook that unparks it
// keeps.
func TestARoutedParkedEventKeepsItsDestination(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonal(t)
	m := newRoutedMachine(t, st)
	root := hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "parked-session"
	sandboxed := m.hook()
	sandboxed.Spool = nil
	r, ok := sandboxed.Open(context.Background(), sid, root)
	if !ok || !sandboxed.EmitFor(r, spool.Event{Name: semconv.TermaSessionEndEvent, SessionID: sid, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "claude-code"}}) {
		t.Fatal("the event was neither spooled nor parked")
	}
	next := m.hook()
	if _, ok := next.Open(context.Background(), sid, root); !ok {
		t.Fatal("the next hook did not resolve")
	}
	events := m.spooled()
	if len(events) != 1 || events[0].Enrollment != hookruntest.Enrollment(t, st, "acme-platform") || events[0].Attrs[AttrProjectID] != nil {
		t.Fatalf("unparked %+v", events)
	}
}

// The events hooks spool themselves rather than through EmitFor (funding evidence,
// observations, a session's name) carry the destination too, and no project.
func TestRoutedEvidenceCarriesItsDestination(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonal(t)
	m := newRoutedMachine(t, st)
	root := hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "evidence-session"
	env := m.hook()
	r, ok := env.Open(context.Background(), sid, root)
	if !ok {
		t.Fatal("not resolved")
	}
	env.CaptureFunding(r, sid, "claude-code", semconv.TermaSessionQuotaEvent, FundingEvidence{Source: "test", Status: StatusPresent})
	env.CaptureObservation(context.Background(), r, Observation{Tool: "cursor", Source: "test", StateDir: "observations", SessionID: sid, Hook: "stop",
		Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "cursor"}})
	SpoolTitle(env, r, "titles", sid, map[string]any{semconv.GenAIMainAgentNameKey: "claude-code"}, func() (string, bool, error) { return "a name", true, nil })
	events := m.spooled()
	if len(events) != 3 {
		t.Fatalf("spooled %s", hookruntest.Names(events))
	}
	for _, ev := range events {
		if ev.Profile != hookruntest.RoutedProfile || ev.Enrollment != hookruntest.Enrollment(t, st, "acme-platform") || ev.OwnLocation || ev.Global {
			t.Errorf("%s: destination %q %q own %v global %v", ev.Name, ev.Profile, ev.Enrollment, ev.OwnLocation, ev.Global)
		}
		if _, ok := ev.Attrs[AttrProjectID]; ok {
			t.Errorf("%s names a project", ev.Name)
		}
	}
}
