package omp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const ompSession = "01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf"

var sessionTitle = Agent{}.Events()["omp-title"]

// titleEnv is an OMP hook whose developer chose OMP at setup.
func titleEnv(t *testing.T) hookrun.Env {
	t.Helper()
	root, stateDir, sp := hookruntest.Project(t)
	return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Version: "test", Policy: hookruntest.Admitting(root),
		Team: hookruntest.Team, Agents: []string{name}}
}

// sendTitle runs omp-title for the name and returns the titles it spooled.
func sendTitle(t *testing.T, env hookrun.Env, title string) []string {
	t.Helper()
	env.Stdin = strings.NewReader(`{"session_id":"` + ompSession + `","cwd":"` + env.Cwd + `","title":"` + title + `"}`)
	if err := sessionTitle(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionTitleEvent) {
		for k, want := range map[string]any{
			semconv.GenAIMainAgentNameKey: name, semconv.TermaEvidenceSourceKey: sourceOmpSession, hookrun.AttrProjectID: hookruntest.Team,
		} {
			if e.Attrs[k] != want {
				t.Errorf("attrs[%q] = %v, want %v", k, e.Attrs[k], want)
			}
		}
		if e.SessionID != ompSession {
			t.Errorf("session %q, want %q", e.SessionID, ompSession)
		}
		titles = append(titles, e.Attrs[semconv.TermaSessionTitleKey].(string))
	}
	return titles
}

// The name OMP gives a session is spooled once, again when renamed, and not for a developer
// who did not choose OMP.
func TestOmpSpoolsTheSessionsName(t *testing.T) {
	env := titleEnv(t)
	if got := sendTitle(t, env, "Fix the flaky build"); len(got) != 1 || got[0] != "Fix the flaky build" {
		t.Fatalf("first name spooled %v", got)
	}
	if got := sendTitle(t, env, "Fix the flaky build"); len(got) != 0 {
		t.Fatalf("an unchanged name was spooled again: %v", got)
	}
	if got := sendTitle(t, env, "Release notes"); len(got) != 1 || got[0] != "Release notes" {
		t.Fatalf("the rename spooled %v", got)
	}
	unchosen := env
	unchosen.Agents = nil
	if got := sendTitle(t, unchosen, "Another name"); len(got) != 0 {
		t.Fatalf("spooled for a developer who did not choose OMP: %v", got)
	}
}

// Anything the title hook cannot use is ignored, never an error.
func TestOmpTitleIgnoresBadInput(t *testing.T) {
	env := titleEnv(t)
	for _, stdin := range []string{"", "not json", `{"title":"x"}`, `{"session_id":"../../etc","title":"x"}`, `{"session_id":"` + ompSession + `","title":"  "}`} {
		env.Stdin = strings.NewReader(stdin)
		if err := sessionTitle(context.Background(), env); err != nil {
			t.Errorf("title(%q) = %v", stdin, err)
		}
	}
	if got := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionTitleEvent); len(got) != 0 {
		t.Fatalf("bad input spooled %+v", got)
	}
}

// Delivery asks the agent again: a title leaves only for a developer who chose OMP, or in
// global mode.
func TestOmpTitleConsent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		consent hookrun.Consent
		want    bool
	}{
		{hookrun.Consent{Agents: []string{name}}, true},
		{hookrun.Consent{Global: true}, true},
		{hookrun.Consent{Agents: []string{"pi"}}, false},
	} {
		if got := (Agent{}).ContentConsented(c.consent); got != c.want {
			t.Errorf("ContentConsented(%+v) = %v, want %v", c.consent, got, c.want)
		}
	}
}
