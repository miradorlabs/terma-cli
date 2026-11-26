package hookrun_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A record that names an agent claims the agent's id for the relay too, for an agent whose
// telemetry uses that id as its session. EmitForSession does not: there the agent is a session
// of its own, whose claim, with its parent and its checkout, another hook wrote. Both claim
// the record's session.
func TestEmitForSessionLeavesTheAgentsClaimAlone(t *testing.T) {
	about := func(session, agent string) spool.Event {
		return spool.Event{Name: semconv.TermaSubagentCallEvent, SessionID: session,
			Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "someagent", semconv.TermaAgentIDKey: agent}}
	}
	t.Run("legacy", func(t *testing.T) {
		root, stateDir, sp := hookruntest.Project(t)
		hookruntest.RelayOn(t, stateDir)
		now := time.Now()
		env := hookrun.Env{StateDir: stateDir, Now: now, Cwd: root, Spool: sp, Team: hookruntest.Team, Policy: hookruntest.Admitting(root), Account: hookruntest.Account}
		r, err := env.Repo(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		env.EmitForSession(r, about("parent-a", "child-a"))
		env.EmitFor(r, about("parent-b", "child-b"))
		for id, want := range map[string]bool{"parent-a": true, "child-a": false, "parent-b": true, "child-b": true} {
			if _, ok := claim.Read(stateDir, id, now); ok != want {
				t.Errorf("claim for %s: %v, want %v", id, ok, want)
			}
		}
		if got := len(hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaSubagentCallEvent)); got != 2 {
			t.Errorf("%d records spooled, want 2", got)
		}
	})
	t.Run("routed", func(t *testing.T) {
		root, stateDir, sp := hookruntest.Project(t)
		hookruntest.RelayOn(t, stateDir)
		now := time.Now()
		origin := "github.com/acme/" + filepath.Base(root)
		st := routing.State{
			Profile: "default",
			Account: hookruntest.Account,
			Teams: []routing.Team{{
				EnrollmentID: "enr-1", OrganizationID: "org-a", AuthURL: "https://auth.example", TeamID: hookruntest.Team, Epoch: 1,
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{origin}, TeamID: hookruntest.Team, FetchedAt: now},
			}},
		}
		env := hookrun.Env{StateDir: stateDir, Now: now, Cwd: root, Spool: sp, Account: hookruntest.Account, Routing: hookrun.NewRouting("default", &st)}
		r, err := env.Repo(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		env.EmitForSession(r, about("parent-a", "child-a"))
		env.EmitFor(r, about("parent-b", "child-b"))
		for id, want := range map[string]bool{"parent-a": true, "child-a": false, "parent-b": true, "child-b": true} {
			if _, ok := claim.Read(stateDir, id, now); ok != want {
				t.Errorf("routed claim for %s: %v, want %v", id, ok, want)
			}
		}
	})
}

// A launch is one marker per (session, agent), {}\n, under the agent's state directory; it
// proves that session's launch alone, ages out like the spool, and a hook with no spool
// records none.
func TestSubagentLaunchEvidence(t *testing.T) {
	stateDir := t.TempDir()
	dir := hookrun.AgentStateDir("someagent", "subagents")
	sp, _ := hookruntest.OpenSpool(t.TempDir())
	now := time.Now()
	env := hookrun.Env{StateDir: stateDir, Spool: sp, Now: now}

	path := hookrun.SubagentEvidencePath(stateDir, dir, "parent", "child")
	if want := filepath.Join(stateDir, dir, hookrun.EvidenceID("parent\x00child")+".json"); path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	env.RememberSubagent(dir, "parent", "child")
	if data, err := os.ReadFile(path); err != nil || string(data) != "{}\n" {
		t.Fatalf("marker %q, %v", data, err)
	}
	if !env.KnownSubagent(dir, "parent", "child") {
		t.Error("the launch is not known")
	}
	for _, other := range [][2]string{{"other", "child"}, {"parent", "other"}} {
		if env.KnownSubagent(dir, other[0], other[1]) {
			t.Errorf("%v is known", other)
		}
	}

	unspooled := hookrun.Env{StateDir: stateDir, Now: now}
	unspooled.RememberSubagent(dir, "parent", "unspooled")
	if env.KnownSubagent(dir, "parent", "unspooled") {
		t.Error("a hook with no spool recorded a launch")
	}

	old := now.Add(-spool.MaxAge - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if env.KnownSubagent(dir, "parent", "child") {
		t.Error("an expired launch is known")
	}
	env.PruneSubagents(dir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the expired marker was not pruned: %v", err)
	}
}
