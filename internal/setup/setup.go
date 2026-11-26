// Package setup is `terma setup`: sign in, choose teams, record the developer's agents,
// activate the chosen teams, point the agents at the relay, and write the machine-wide
// hooks.
package setup

import (
	"context"
	"errors"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Steps are what setup reaches through the command line: sign-in, prompts, the network
// and the relay. SignIn, ChooseAgents and SetupTeams are required; the rest do nothing
// when nil.
type Steps struct {
	SignIn func(ctx context.Context, cfg *config.Config) (*config.Config, error)
	// ChooseTeams asks which teams to send to, before the agents are chosen.
	ChooseTeams  func(ctx context.Context) error
	ChooseAgents func(ctx context.Context, cfg *config.Config) ([]string, error)
	// Recorded says which agents were recorded, before teams are activated.
	Recorded func(names []string)
	// SetupTeams verifies no pre-routing relay runs, selects and refreshes teams via CAS
	// routing.State, persists committed team policies, and returns the primary team's policy.
	SetupTeams func(ctx context.Context, cfg *config.Config) (config.Policy, error)
	// SpoolKey makes sure the team's key for hook events is on this machine.
	SpoolKey     func(ctx context.Context, cfg *config.Config)
	ConnectRelay func(ctx context.Context, names []string) error
	// MachineHooks writes the agents' machine-wide hooks and git's global hooks path.
	MachineHooks func(ctx context.Context, names []string) error
	// CheckIn asks the relay for a heartbeat once agents are chosen: ConnectRelay starts it
	// for any agent, since an agent that reports through its hooks alone needs its token.
	CheckIn func(ctx context.Context)
}

// Result is what a setup recorded.
type Result struct {
	Agents []string
	Policy config.Policy
}

// Run sets the machine up for cfg's profile. The agents are recorded before teams are
// activated, and routing state is committed before anything is pointed at the relay.
func Run(ctx context.Context, cfg *config.Config, s Steps) (Result, error) {
	if s.SignIn == nil || s.ChooseAgents == nil || s.SetupTeams == nil {
		return Result{}, errors.New("setup: sign-in, the agent choice and team setup are required")
	}
	cfg, err := s.SignIn(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	if s.ChooseTeams != nil {
		if err := s.ChooseTeams(ctx); err != nil {
			return Result{}, err
		}
	}
	names, err := s.ChooseAgents(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	if err := config.UpdateProfile(cfg.Dir, cfg.ProfileName, func(p *config.Profile) { p.Harnesses = names }); err != nil {
		return Result{}, err
	}
	call(s.Recorded, names)

	pol, err := s.SetupTeams(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	cfg.Policy = pol
	if s.SpoolKey != nil {
		s.SpoolKey(ctx, cfg)
	}

	if s.ConnectRelay != nil {
		if err := s.ConnectRelay(ctx, names); err != nil {
			return Result{}, err
		}
	}
	if s.MachineHooks != nil {
		if err := s.MachineHooks(ctx, names); err != nil {
			return Result{}, err
		}
	}
	if len(names) > 0 && s.CheckIn != nil {
		s.CheckIn(ctx)
	}
	return Result{Agents: names, Policy: pol}, nil
}

func call[T any](f func(T), v T) {
	if f != nil {
		f(v)
	}
}
