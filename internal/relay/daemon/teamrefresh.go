package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// RefusedRecheck is how often a Refused team's policy is asked for again: once a day. If it
// comes back, the team is Active again under the same enrollment, and is given a new key.
const RefusedRecheck = 24 * time.Hour

// teamRefresher keeps every enrollment in the profile's routing state fresh, each with its
// own organization's sign-in, never the keys stored here. A result is applied through
// routing.Update with the epoch its request started in (routing.State.ApplyRefresh), so a
// success already in flight never undoes a refusal. A team made Refused is published
// first, so every sender stops, and only then is its key deleted. A Refused team is asked
// again once a day.
func (d Deps) teamRefresher() *PolicyRefresher {
	return &PolicyRefresher{
		Interval: PolicyRefreshInterval,
		Discover: 5 * time.Second,
		Teams: func() []string {
			_, st, ok := d.committed()
			if !ok {
				return nil
			}
			ids := make([]string, len(st.Teams))
			for i, t := range st.Teams {
				ids[i] = t.EnrollmentID
			}
			return ids
		},
		Fetched: func(enrollment string) time.Time {
			_, st, ok := d.committed()
			t := team(st, enrollment)
			switch {
			case !ok || t == nil:
				return time.Time{}
			case t.StateAt(time.Now()) == routing.Refused:
				return t.RefusedAt
			}
			return t.Policy.FetchedAt
		},
		Every: func(enrollment string) time.Duration {
			if _, st, ok := d.committed(); ok {
				if t := team(st, enrollment); t != nil && t.StateAt(time.Now()) == routing.Refused {
					return RefusedRecheck
				}
			}
			return PolicyRefreshInterval
		},
		Refresh: d.refreshTeam,
	}
}

// committed is the profile's configuration and its committed routing state.
func (d Deps) committed() (*config.Config, routing.State, bool) {
	if d.LoadConfig == nil {
		return nil, routing.State{}, false
	}
	cfg, err := d.LoadConfig()
	if err != nil {
		return nil, routing.State{}, false
	}
	st, ok, err := routing.LoadReady(cfg.StateDir, cfg.ProfileName)
	if err != nil || !ok {
		return cfg, routing.State{}, false
	}
	return cfg, st, true
}

// refreshTeam fetches enrollment's policy and applies it, or its failure's class, to the
// routing state; a team that is now Refused loses its key on this machine.
func (d Deps) refreshTeam(ctx context.Context, enrollment string) error {
	cfg, st, ok := d.committed()
	t := team(st, enrollment)
	if !ok || t == nil || d.RefreshTeam == nil {
		return nil
	}
	r, fetchErr := d.RefreshTeam(ctx, cfg, *t)
	var refused bool
	_, err := routing.Update(cfg.StateDir, cfg.ProfileName, func(s *routing.State) error {
		if _, err := s.ApplyRefresh(r); err != nil {
			return err
		}
		now := team(*s, enrollment)
		refused = now != nil && now.StateAt(time.Now()) == routing.Refused
		return nil
	})
	if err == nil && refused {
		err = keystore.DeleteTeam(cfg.Dir, enrollment)
	}
	return errors.Join(fetchErr, err)
}
