package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

type routedFake struct {
	fn agents.Handler
}

func (routedFake) Name() string                   { return "rfake" }
func (routedFake) DisplayName() string            { return "RFake" }
func (routedFake) Installed(context.Context) bool { return true }
func (routedFake) FlushAfter() []string           { return nil }
func (r routedFake) Events() map[string]agents.Handler {
	return map[string]agents.Handler{"rfake-hook": r.fn}
}

func TestDispatchUsesClaimedRoutedWhenRoutingActive(t *testing.T) {
	stateDir := t.TempDir()
	hookruntest.RelayOn(t, stateDir)
	rt := hookrun.NewRouting("default", nil)
	var routedCalled bool
	var gotRT *hookrun.Routing

	deps := Deps{
		StateDir: stateDir,
		Agents: agents.New(routedFake{fn: func(_ context.Context, env hookrun.Env) error {
			if env.Routing != rt {
				t.Errorf("env.Routing = %v; want %v", env.Routing, rt)
			}
			if env.OnClaim != nil {
				env.OnClaim("/tmp/repo")
			}
			return nil
		}}),
		Profile:       func() Profile { return Profile{Routing: rt} },
		Spool:         func() *spool.Spool { return nil },
		ClaimedRouted: func(_ string, got *hookrun.Routing) { routedCalled, gotRT = true, got },
	}
	status := Run(context.Background(), deps, Request{Event: "rfake-hook", Cwd: t.TempDir(), Stdin: strings.NewReader("{}")})
	if status != 0 {
		t.Fatalf("status = %d; want 0", status)
	}
	if !routedCalled || gotRT != rt {
		t.Fatalf("routed=%v gotRT=%v; want true, %v", routedCalled, gotRT, rt)
	}
}

type extFake struct{}

func (extFake) Name() string                   { return "extfake" }
func (extFake) DisplayName() string            { return "ExtFake" }
func (extFake) Installed(context.Context) bool { return true }
func (extFake) FlushAfter() []string           { return nil }
func (extFake) Events() map[string]agents.Handler {
	ext := hookrun.Extension{Tool: "omp"}
	ev := ext.Events("extfake")
	ev["extfake-verify-prompt"] = ext.VerifyPrompt
	return ev
}

// A plugin installed by an earlier build still sends <tool>-tool-reach until setup refreshes
// it: the hook takes it, writes nothing and exits 0, routed or not.
func TestDispatchLegacyToolReachIsANoOp(t *testing.T) {
	stateDir := t.TempDir()
	hookruntest.RelayOn(t, stateDir)
	repo := t.TempDir()
	body := `{"session_id":"018f3a2c-9109-7000-8000-000000000001","cwd":` + strconv.Quote(repo) + `,"tool":"read","paths":["/elsewhere/secret.go"]}`
	for _, rt := range []*hookrun.Routing{nil, hookrun.NewRouting("default", nil)} {
		var claimed bool
		deps := Deps{
			StateDir:      stateDir,
			Agents:        agents.New(extFake{}),
			Profile:       func() Profile { return Profile{Routing: rt} },
			Spool:         func() *spool.Spool { return nil },
			ClaimedRouted: func(string, *hookrun.Routing) { claimed = true },
		}
		if code := Run(context.Background(), deps, Request{Event: "extfake-tool-reach", Cwd: repo, Stdin: strings.NewReader(body)}); code != 0 {
			t.Fatalf("extfake-tool-reach (routing %v) exited %d; want 0", rt != nil, code)
		}
		if claimed {
			t.Fatalf("extfake-tool-reach (routing %v) claimed the session", rt != nil)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, "runs"))
	if len(entries) != 0 {
		t.Fatalf("extfake-tool-reach wrote run state: %v", entries)
	}
}

func TestDispatchVerifyPromptFailsClosedWhenClaimLocked(t *testing.T) {
	stateDir := t.TempDir()
	hookruntest.RelayOn(t, stateDir)
	repoA := t.TempDir()
	repoB := t.TempDir()
	for _, pair := range [][2]string{{repoA, "git@github.com:acme/api.git"}, {repoB, "git@github.com:beta/core.git"}} {
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", pair[1]}} {
			cmd := exec.Command("git", append([]string{"-C", pair[0]}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	now := time.Now().UTC()
	st := routing.State{
		Account: "acct-test",
		Profile: "default",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/api"}, OrganizationID: "org-a", TeamID: "team-a", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/beta/core"}, OrganizationID: "org-b", TeamID: "team-b", FetchedAt: now},
			},
		},
	}
	rt := hookrun.NewRouting("default", &st)
	deps := Deps{
		StateDir: stateDir,
		Agents:   agents.New(extFake{}),
		Profile:  func() Profile { return Profile{Routing: rt, Account: "acct-test"} },
		Spool:    func() *spool.Spool { return nil },
	}
	sid := "018f3a2c-9109-7000-8000-000000000099"
	runPrompt := func(cwd string) int {
		body := `{"session_id":` + strconv.Quote(sid) + `,"cwd":` + strconv.Quote(cwd) + `}`
		return Run(context.Background(), deps, Request{Event: "extfake-verify-prompt", Cwd: cwd, Stdin: strings.NewReader(body)})
	}
	if code := runPrompt(repoA); code != 0 {
		t.Fatalf("initial placement in repoA exited %d; want 0", code)
	}
	if code := runPrompt(repoA); code != 0 {
		t.Fatalf("same-place refresh in repoA exited %d; want 0", code)
	}
	if code := runPrompt(repoB); code != 0 {
		t.Fatalf("move placement to repoB exited %d; want 0", code)
	}
	c, ok := claim.Read(stateDir, sid, time.Now())
	if !ok || c.Repository.Origin != "github.com/beta/core" {
		t.Fatalf("claim after move = (%+v, %v); want beta/core", c, ok)
	}
	// Hold the claim's sidecar lock so moving back to repoA cannot persist the new placement.
	lockPath := filepath.Join(claim.Dir(stateDir), "claims", sid+".json.lock")
	unlock, err := flock.Lock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock claim: %v", err)
	}
	defer unlock()
	if code := runPrompt(repoA); code != 1 {
		t.Fatalf("move placement while claim locked exited %d; want 1", code)
	}
	c, ok = claim.Read(stateDir, sid, time.Now())
	if !ok || c.Repository.Origin != "github.com/beta/core" {
		t.Fatalf("claim changed while locked: (%+v, %v)", c, ok)
	}
}
