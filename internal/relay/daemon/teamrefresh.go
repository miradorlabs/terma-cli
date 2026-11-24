package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// RefusedRecheck is how often a Refused enrollment is refreshed again to detect restored
// membership or organization access.
const RefusedRecheck = 24 * time.Hour

// BindTeams wires daemon.Teams and its ordered CAS TeamRefresher into opts, returning the
// refresher worker and an OnRelay hook so refresh transitions invalidate cached state and
// immediately wake affected teamSenders past refusedBackoff.
func BindTeams(d Deps, cfg *config.Config, opts *relay.Options) (worker func(context.Context), onRelay func(*relay.Relay)) {
	teams := d.Teams(cfg)
	var activeRelay atomic.Pointer[relay.Relay]
	ref := d.TeamRefresher(func(enrollments ...string) {
		teams.Invalidate()
		if r := activeRelay.Load(); r != nil {
			r.OnStateChange(enrollments...)
		}
	})
	opts.Router = teams
	opts.NudgeRefresh = ref.Nudge
	return ref.Run, func(r *relay.Relay) { activeRelay.Store(r) }
}

// TeamRefresher keeps every selected team's collection policy fresh while the relay runs,
// skipping and failing closed when State.Account != Profile.Account, checking Refused
// teams every RefusedRecheck, and committing every result through CAS routing.Update with
// pre-request per-enrollment epoch snapshots.
func (d Deps) TeamRefresher(onChange func(enrollments ...string)) *PolicyRefresher {
	var ref *PolicyRefresher
	clock := func() time.Time {
		if ref != nil && ref.Now != nil {
			return ref.Now()
		}
		return time.Now()
	}
	ref = &PolicyRefresher{
		Interval: PolicyRefreshInterval,
		Discover: 5 * time.Second,
		Teams: func() []string {
			_, st, ok := d.committedRouting()
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
			_, st, ok := d.committedRouting()
			if !ok {
				return time.Time{}
			}
			t, has := st.Team(enrollment)
			switch {
			case !has:
				return time.Time{}
			case t.StateAt(clock()) == routing.Refused:
				return t.RefusedAt
			}
			return t.Policy.FetchedAt
		},
		Every: func(enrollment string) time.Duration {
			if _, st, ok := d.committedRouting(); ok {
				if t, has := st.Team(enrollment); has && t.StateAt(clock()) == routing.Refused {
					return RefusedRecheck
				}
			}
			return PolicyRefreshInterval
		},
		Refresh: func(ctx context.Context, enrollment string) error {
			return d.refreshTeam(ctx, enrollment, onChange)
		},
	}
	return ref
}

// committedRouting loads the profile's config and routing.State, failing closed (ok=false)
// when no routing state exists, it cannot be read, or State.Account != expectedAccount(cfg).
func (d Deps) committedRouting() (*config.Config, routing.State, bool) {
	if d.LoadConfig == nil {
		return nil, routing.State{}, false
	}
	cfg, err := d.LoadConfig()
	if err != nil {
		return nil, routing.State{}, false
	}
	st, ok, err := routing.Load(cfg.StateDir, cfg.ProfileName)
	want := expectedAccount(cfg)
	if err != nil || !ok || want == "" || st.Account != want {
		return cfg, routing.State{}, false
	}
	return cfg, st, true
}

func (d Deps) refreshTeam(ctx context.Context, enrollment string, onChange func(...string)) error {
	cfg, st, ok := d.committedRouting()
	if !ok || d.RefreshTeam == nil {
		return nil
	}
	t, has := st.Team(enrollment)
	if !has {
		return nil
	}
	epochs := map[string]int64{}
	for _, tm := range st.Teams {
		if tm.OrganizationID == t.OrganizationID && tm.AuthURL == t.AuthURL {
			epochs[tm.EnrollmentID] = tm.Epoch
		}
	}
	r, fetchErr := d.RefreshTeam(ctx, cfg, st.Account, t)
	var applied bool
	var changed []string
	committed, err := routing.Update(cfg.StateDir, cfg.ProfileName, routing.Expect{Account: st.Account}, func(s *routing.State) error {
		changed = nil
		if r.Failure == routing.OrgAccessLost && t.OrganizationID != "" && t.AuthURL != "" {
			for _, tm := range s.Teams {
				if tm.OrganizationID == t.OrganizationID && tm.AuthURL == t.AuthURL {
					if want, ok := epochs[tm.EnrollmentID]; ok && tm.Epoch == want {
						changed = append(changed, tm.EnrollmentID)
					}
				}
			}
			applied = s.ApplyOrgAccessLost(t.OrganizationID, t.AuthURL, epochs, r.At)
			if !applied {
				changed = nil
			}
			return nil
		}
		var uerr error
		applied, uerr = s.ApplyRefresh(r)
		if uerr != nil {
			return uerr
		}
		if applied && (r.Policy != nil || r.Failure != routing.Transient) {
			changed = []string{enrollment}
		}
		return nil
	})
	if err == nil && applied {
		if r.Policy != nil {
			if tm, has := committed.Team(enrollment); has {
				_ = routing.StoreTeamPolicy(cfg.StateDir, tm)
			}
		}
		if len(changed) > 0 && onChange != nil {
			onChange(changed...)
		}
	}
	return errors.Join(fetchErr, err)
}
