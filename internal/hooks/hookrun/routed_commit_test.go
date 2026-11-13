package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

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
