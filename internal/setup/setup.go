// Package setup is `terma setup`: sign in, record the developer's agents, fetch the
// selected team's collection policy, point the agents at the relay, and write the
// machine-wide hooks.
package setup

import (
	"context"
	"errors"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// ErrServerKey is setup run under a server key: it signs in as a person.
var ErrServerKey = errors.New("TERMA_API_KEY is set — setup signs in as a person; unset it first")

// Steps are what setup reaches through the command line: sign-in, prompts, the network
// and the relay. SignIn, ChooseAgents, SelectTeam and FetchPolicy are required; the rest
// do nothing when nil.
type Steps struct {
	SignIn       func(ctx context.Context, cfg *config.Config) (*config.Config, error)
	ChooseAgents func(ctx context.Context, cfg *config.Config) ([]string, error)
	SelectTeam   func(ctx context.Context, cfg *config.Config) error
	FetchPolicy  func(ctx context.Context, cfg *config.Config) (config.Policy, error)
	// Recorded says which agents were recorded, before the policy is fetched.
	Recorded func(names []string)
	// StopRelay stops a running relay whose login or scope the new policy changed; a
	// relay fixes both at startup.
	StopRelay func()
	// Fetched says which policy is now in force.
	Fetched func(config.Policy)
	// SpoolKey makes sure the team's key for hook events is on this machine.
	SpoolKey     func(ctx context.Context, cfg *config.Config)
	ConnectRelay func(ctx context.Context, names []string) error
	// MachineHooks writes the agents' machine-wide hooks and git's global hooks path.
	MachineHooks func(ctx context.Context, names []string) error
	// CheckIn asks the relay for a heartbeat, once an agent reports through it.
	CheckIn func(ctx context.Context)
}

// Result is what a setup recorded.
type Result struct {
	Agents []string
	Policy config.Policy
}

// Run sets the machine up for cfg's profile. The agents are recorded before the policy
// is fetched, and the policy is stored before anything is pointed at the relay.
func Run(ctx context.Context, reg *agents.Registry, cfg *config.Config, s Steps) (Result, error) {
	if s.SignIn == nil || s.ChooseAgents == nil || s.SelectTeam == nil || s.FetchPolicy == nil {
		return Result{}, errors.New("setup: sign-in, the agent choice, the team and the policy fetch are required")
	}
	if cfg.APIKey != "" {
		return Result{}, ErrServerKey
	}
	cfg, err := s.SignIn(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	names, err := s.ChooseAgents(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	if err := config.UpdateProfile(cfg.Dir, cfg.ProfileName, func(p *config.Profile) { p.Harnesses = names }); err != nil {
		return Result{}, err
	}
	call(s.Recorded, names)

	// Kept in the state directory: hooks and the relay read policy there, never the network.
	if err := s.SelectTeam(ctx, cfg); err != nil {
		return Result{}, err
	}
	pol, err := s.FetchPolicy(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	previous := cfg.Policy
	cfg.Policy = pol
	if err := routing.StorePolicy(cfg, &pol); err != nil {
		return Result{}, err
	}
	if previous.OrganizationID != pol.OrganizationID || previous.AuthURL != pol.AuthURL || previous.TeamID != pol.TeamID {
		if s.StopRelay != nil {
			s.StopRelay()
		}
	}
	call(s.Fetched, pol)
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
	if len(reg.RelayTargets(names)) > 0 && s.CheckIn != nil {
		s.CheckIn(ctx)
	}
	return Result{Agents: names, Policy: pol}, nil
}

func call[T any](f func(T), v T) {
	if f != nil {
		f(v)
	}
}
