package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/session"
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
	return hookrun.Extension{Tool: "omp"}.Events("extfake")
}

type nativeReachFake struct {
	event string
	tool  string
	path  string
	sid   string
}

func (n nativeReachFake) Name() string                   { return n.tool }
func (n nativeReachFake) DisplayName() string            { return n.tool }
func (n nativeReachFake) Installed(context.Context) bool { return true }
func (n nativeReachFake) FlushAfter() []string           { return []string{n.event} }
func (n nativeReachFake) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		n.event: func(ctx context.Context, env hookrun.Env) error {
			env.Classify(ctx, nil, session.Session{ID: n.sid, Tool: n.tool}, []string{n.path}, nil)
			return nil
		},
	}
}

func TestDispatchToolReachFailsClosedOnWriteError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX non-root chmod permission test")
	}
	stateDir := t.TempDir()
	hookruntest.RelayOn(t, stateDir)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	repoA, repoB := filepath.Join(parent, "alpha"), filepath.Join(parent, "beta")
	for _, r := range [][2]string{{repoA, "git@github.com:orgA/alpha.git"}, {repoB, "git@github.com:orgB/beta.git"}} {
		_, _ = gitx.Git(context.Background(), parent, "init", "-q", "-b", "main", r[0])
		_, _ = gitx.Git(context.Background(), r[0], "remote", "add", "origin", r[1])
	}
	fileB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const acct = "1111111111111111"
	now := time.Now().UTC().Truncate(time.Second)
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		for _, spec := range [][3]string{{"orgA", "team-a", "github.com/orgA/alpha"}, {"orgB", "team-b", "github.com/orgB/beta"}} {
			if _, err := s.Select(routing.Team{OrganizationID: spec[0], AuthURL: "https://api.example.com", TeamID: spec[1],
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{spec[2]}, Revision: 1, FetchedAt: now, TeamID: spec[1], OrganizationID: spec[0], AuthURL: "https://api.example.com"}}); err != nil {
				return err
			}
			if err := s.Bind(spec[2], spec[0], false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := hookrun.NewRouting("default", &st)
	deps := Deps{
		StateDir: stateDir,
		Agents:   agents.New(extFake{}),
		Profile:  func() Profile { return Profile{Routing: rt, Account: acct} },
		Spool:    func() *spool.Spool { return nil },
	}
	ctx := context.Background()
	sid := "018f3a2c-9109-7000-8000-000000000001"
	if code := Run(ctx, deps, Request{Event: "extfake-session-start", Cwd: repoA, Stdin: strings.NewReader(`{"session_id":"` + sid + `","cwd":` + strconv.Quote(repoA) + `}`)}); code != 0 {
		t.Fatalf("session-start code = %d; want 0", code)
	}
	// Make runs/ and claims/ readable (stale clean claim remains readable) but unwritable.
	runsDir := filepath.Join(stateDir, "runs")
	claimsDir := filepath.Join(stateDir, "claims")
	_ = os.MkdirAll(claimsDir, 0o700)
	if err := os.Chmod(runsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(claimsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(runsDir, 0o700)
		_ = os.Chmod(claimsDir, 0o700)
	})
	// Cross-org reach must exit non-zero when persistence fails.
	reachBody := `{"session_id":"` + sid + `","cwd":` + strconv.Quote(repoA) + `,"tool":"read","paths":[` + strconv.Quote(fileB) + `]}`
	if code := Run(ctx, deps, Request{Event: "extfake-tool-reach", Cwd: repoA, Stdin: strings.NewReader(reachBody)}); code == 0 {
		t.Fatalf("extfake-tool-reach exited 0 despite unwritable stateDir; want non-zero")
	}
	// Unknown-reach fallback fence must also exit non-zero when persistence fails.
	fenceBody := `{"session_id":"` + sid + `","cwd":` + strconv.Quote(repoA) + `,"tool":"bash","command":"eval $TERMA_REACH_FAILED"}`
	if code := Run(ctx, deps, Request{Event: "extfake-tool-reach", Cwd: repoA, Stdin: strings.NewReader(fenceBody)}); code == 0 {
		t.Fatalf("fallback fence exited 0 despite unwritable stateDir; want non-zero")
	}
	// Local read inside repoB triggers fallback ClaimFromPayload crossing orgA->orgB;
	// persistence failure during fallback claiming must also exit non-zero.
	localInB := `{"session_id":"` + sid + `","cwd":` + strconv.Quote(repoB) + `,"tool":"read","paths":[` + strconv.Quote(fileB) + `]}`
	if code := Run(ctx, deps, Request{Event: "extfake-tool-reach", Cwd: repoB, Stdin: strings.NewReader(localInB)}); code == 0 {
		t.Fatalf("fallback ClaimFromPayload tool-reach exited 0 despite unwritable stateDir; want non-zero")
	}
	// Native pre-tool reach hooks (Claude pre-tool-use and Codex codex-pre-tool-use)
	// must also exit non-zero and skip ClaimedRouted/Flush when persistence fails.
	for _, tc := range []struct {
		event, tool string
	}{
		{"pre-tool-use", "claude-code"},
		{"codex-pre-tool-use", "codex"},
	} {
		var claimed, flushed bool
		nativeDeps := Deps{
			StateDir:      stateDir,
			Agents:        agents.New(nativeReachFake{event: tc.event, tool: tc.tool, path: fileB, sid: sid}),
			Profile:       func() Profile { return Profile{Routing: hookrun.NewRouting("default", &st), Account: acct} },
			Spool:         func() *spool.Spool { return nil },
			ClaimedRouted: func(string, *hookrun.Routing) { claimed = true },
			Flush:         func() { flushed = true },
		}
		if code := Run(ctx, nativeDeps, Request{Event: tc.event, Cwd: repoA, Stdin: strings.NewReader("{}")}); code == 0 {
			t.Fatalf("%s exited 0 despite unwritable stateDir; want non-zero", tc.event)
		}
		if claimed || flushed {
			t.Fatalf("%s invoked ClaimedRouted=%v Flush=%v on persistence failure", tc.event, claimed, flushed)
		}
	}
	// Unusable routing state also fails closed with non-zero exit.
	unusableDeps := deps
	unusableDeps.Profile = func() Profile {
		return Profile{Routing: hookrun.NewRouting("default", nil)}
	}
	if code := Run(ctx, unusableDeps, Request{Event: "extfake-tool-reach", Cwd: repoA, Stdin: strings.NewReader(reachBody)}); code == 0 {
		t.Fatalf("unusable routing tool-reach exited 0; want non-zero")
	}
}
