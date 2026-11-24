package setup

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func steps(log *[]string, pol config.Policy) Steps {
	note := func(s string) { *log = append(*log, s) }
	return Steps{
		SignIn:       func(_ context.Context, cfg *config.Config) (*config.Config, error) { note("sign-in"); return cfg, nil },
		ChooseAgents: func(context.Context, *config.Config) ([]string, error) { note("choose"); return []string{"fake"}, nil },
		Recorded:     func([]string) { note("recorded") },
		SetupTeams:   func(context.Context, *config.Config) (config.Policy, error) { note("teams"); return pol, nil },
		ConnectRelay: func(context.Context, []string) error { note("relay"); return nil },
		SpoolKey:     func(context.Context, *config.Config) { note("spool-key") },
		MachineHooks: func(context.Context, []string) error { note("machine-hooks"); return nil },
		CheckIn:      func(context.Context) { note("check-in") },
	}
}

func policy() config.Policy {
	return config.Policy{Mode: config.ModeRepo, OrganizationID: "org_a", AuthURL: "https://auth.example", TeamID: "t1", FetchedAt: time.Now()}
}

// The agents are recorded before teams are activated, and routing state is committed
// before the agents are pointed at the relay.
func TestSetupRecordsThenActivatesThenConnects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var log []string
	res, err := Run(t.Context(), &config.Config{Dir: dir, StateDir: dir, OrganizationID: "org_a", AuthURL: "https://auth.example"}, steps(&log, policy()))
	if err != nil {
		t.Fatal(err)
	}
	// Any chosen agent checks in, one with no relay exporter too: the relay runs for its hooks.
	want := []string{"sign-in", "choose", "recorded", "teams", "spool-key", "relay", "machine-hooks", "check-in"}
	if !slices.Equal(log, want) {
		t.Fatalf("steps = %v, want %v", log, want)
	}
	if !slices.Equal(res.Agents, []string{"fake"}) || res.Policy.TeamID != "t1" {
		t.Fatalf("Result = %+v", res)
	}
	file, err := config.LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := file.Profiles[config.DefaultProfile]; p == nil || !slices.Equal(p.Harnesses, []string{"fake"}) {
		t.Fatalf("profile = %+v", p)
	}
}

// Setup never runs without its sign-in or team setup, and a cancelled choice records nothing.
func TestSetupRefusesWhatItCannotDoSafely(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var log []string
	missing := steps(&log, policy())
	missing.SetupTeams = nil
	if _, err := Run(t.Context(), &config.Config{Dir: dir, StateDir: dir}, missing); err == nil {
		t.Fatal("ran without team setup")
	}
	cancelled := errors.New("cancelled")
	s := steps(&log, policy())
	s.ChooseAgents = func(context.Context, *config.Config) ([]string, error) { return nil, cancelled }
	log = nil
	if _, err := Run(t.Context(), &config.Config{Dir: dir, StateDir: dir}, s); !errors.Is(err, cancelled) {
		t.Fatalf("a cancelled choice: %v", err)
	}
	if file, _ := config.LoadFile(dir); file != nil && file.Profiles[config.DefaultProfile] != nil {
		t.Fatal("a cancelled setup recorded the profile")
	}
	if slices.Contains(log, "teams") {
		t.Fatalf("a cancelled setup carried on: %v", log)
	}
}
