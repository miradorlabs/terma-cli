package setup

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

func steps(log *[]string, pol config.Policy) Steps {
	note := func(s string) { *log = append(*log, s) }
	return Steps{
		SignIn:       func(_ context.Context, cfg *config.Config) (*config.Config, error) { note("sign-in"); return cfg, nil },
		ChooseAgents: func(context.Context, *config.Config) ([]string, error) { note("choose"); return []string{"fake"}, nil },
		Recorded:     func([]string) { note("recorded") },
		SelectTeam:   func(context.Context, *config.Config) error { note("team"); return nil },
		FetchPolicy:  func(context.Context, *config.Config) (config.Policy, error) { note("fetch"); return pol, nil },
		StopRelay:    func() { note("stop-relay") },
		Fetched:      func(config.Policy) { note("fetched") },
		ConnectRelay: func(context.Context, []string) error {
			if _, ok, _ := routing.LoadPolicy(pol.TeamID); !ok {
				note("relay-before-policy")
			}
			note("relay")
			return nil
		},
		ApplyMode: func(_ context.Context, _ []string, global bool) error { note("mode"); return nil },
		CheckIn:   func(context.Context) { note("check-in") },
	}
}

func policy() config.Policy {
	return config.Policy{Mode: config.ModeRepo, Signals: []string{"logs"}, OrganizationID: "org_a", AuthURL: "https://auth.example", TeamID: "t1", FetchedAt: time.Now()}
}

// The agents are recorded before the policy is fetched, the policy is stored before the
// agents are pointed at the relay, and a relay is stopped when its scope changed.
func TestSetupRecordsThenFetchesThenConnects(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	var log []string
	reg := agents.New(agentstest.Agent{ID: "fake"})
	res, err := Run(t.Context(), reg, &config.Config{OrganizationID: "org_a", AuthURL: "https://auth.example"}, steps(&log, policy()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sign-in", "choose", "recorded", "team", "fetch", "stop-relay", "fetched", "relay", "mode"}
	if !slices.Equal(log, want) {
		t.Fatalf("steps = %v, want %v", log, want)
	}
	if !slices.Equal(res.Agents, []string{"fake"}) || res.Policy.TeamID != "t1" {
		t.Fatalf("Result = %+v", res)
	}
	file, err := config.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if p := file.Profiles[config.DefaultProfile]; p == nil || !slices.Equal(p.Harnesses, []string{"fake"}) {
		t.Fatalf("profile = %+v", p)
	}
}

// A relay already running for this team keeps running: only its login or scope fixes it.
func TestSetupLeavesARelayOfTheSameScopeRunning(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	var log []string
	pol := policy()
	_, err := Run(t.Context(), agents.New(), &config.Config{OrganizationID: "org_a", AuthURL: pol.AuthURL, Policy: pol}, steps(&log, pol))
	if err != nil || slices.Contains(log, "stop-relay") {
		t.Fatalf("steps = %v, err %v", log, err)
	}
}

// Setup never runs without its sign-in or policy, never under a server key, and a
// cancelled choice records nothing.
func TestSetupRefusesWhatItCannotDoSafely(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	var log []string
	missing := steps(&log, policy())
	missing.FetchPolicy = nil
	if _, err := Run(t.Context(), agents.New(), &config.Config{}, missing); err == nil {
		t.Fatal("ran without a policy fetch")
	}
	if _, err := Run(t.Context(), agents.New(), &config.Config{APIKey: "ter_srv_x"}, steps(&log, policy())); !errors.Is(err, ErrServerKey) {
		t.Fatalf("under a server key: %v", err)
	}
	cancelled := errors.New("cancelled")
	s := steps(&log, policy())
	s.ChooseAgents = func(context.Context, *config.Config) ([]string, error) { return nil, cancelled }
	log = nil
	if _, err := Run(t.Context(), agents.New(), &config.Config{}, s); !errors.Is(err, cancelled) {
		t.Fatalf("a cancelled choice: %v", err)
	}
	if file, _ := config.LoadFile(); file != nil && file.Profiles[config.DefaultProfile] != nil {
		t.Fatal("a cancelled setup recorded the profile")
	}
	if slices.Contains(log, "fetch") {
		t.Fatalf("a cancelled setup carried on: %v", log)
	}
}
