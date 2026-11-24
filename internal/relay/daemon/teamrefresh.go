package daemon

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
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

// TeamRefresher keeps every selected team's collection policy fresh while the relay runs
// across the active profile and any other configured profile whose routing state is
// account-verified (State.Account == expectedAccount(cfg)). That ensures queued outbox
// entries from a previous profile keep refreshing after an active-profile switch, while
// failing closed on account mismatches, checking Refused teams every RefusedRecheck, and
// committing every result through CAS routing.Update with pre-request per-enrollment
// epoch snapshots.
func (d Deps) TeamRefresher(onChange func(enrollments ...string)) *PolicyRefresher {
	var ref *PolicyRefresher
	clock := func() time.Time {
		if ref != nil && ref.Now != nil {
			return ref.Now()
		}
		return time.Now()
	}
	var ownerMu sync.Mutex
	owners := map[string]string{}
	find := func(enrollment string) (committedProfile, routing.Team, bool) {
		ownerMu.Lock()
		hint := owners[enrollment]
		ownerMu.Unlock()
		cp, t, ok := d.findCommitted(enrollment, hint)
		if ok {
			ownerMu.Lock()
			owners[enrollment] = cp.cfg.ProfileName
			ownerMu.Unlock()
		}
		return cp, t, ok
	}
	ref = &PolicyRefresher{
		Interval: PolicyRefreshInterval,
		Discover: 5 * time.Second,
		Teams: func() []string {
			profiles := d.committedRoutings()
			if len(profiles) == 0 {
				return nil
			}
			var ids []string
			seen := map[string]bool{}
			ownerMu.Lock()
			for _, cp := range profiles {
				for _, t := range cp.st.Teams {
					if seen[t.EnrollmentID] {
						continue
					}
					seen[t.EnrollmentID] = true
					owners[t.EnrollmentID] = cp.cfg.ProfileName
					ids = append(ids, t.EnrollmentID)
				}
			}
			ownerMu.Unlock()
			return ids
		},
		Fetched: func(enrollment string) time.Time {
			_, t, ok := find(enrollment)
			if !ok {
				return time.Time{}
			}
			if t.StateAt(clock()) == routing.Refused {
				return t.RefusedAt
			}
			return t.Policy.FetchedAt
		},
		Every: func(enrollment string) time.Duration {
			if _, t, ok := find(enrollment); ok && t.StateAt(clock()) == routing.Refused {
				return RefusedRecheck
			}
			return PolicyRefreshInterval
		},
		Refresh: func(ctx context.Context, enrollment string) error {
			cp, t, ok := find(enrollment)
			if !ok || d.RefreshTeam == nil {
				return nil
			}
			return d.refreshTeam(ctx, cp, t, onChange)
		},
	}
	return ref
}

type committedProfile struct {
	cfg *config.Config
	st  routing.State
}

// committedFor loads name's config and routing.State, failing closed (ok=false) when the
// profile or routing state cannot be read or State.Account != expectedAccount(cfg).
func (d Deps) committedFor(name string, active *config.Config) (committedProfile, bool) {
	cfg := active
	if active == nil || name != active.ProfileName {
		var err error
		switch {
		case d.LoadProfile != nil:
			cfg, err = d.LoadProfile(name)
		case active != nil && active.Dir != "":
			cfg, err = config.Load(active.Dir, active.StateDir, config.Overrides{Profile: name})
		default:
			return committedProfile{}, false
		}
		if err != nil || cfg == nil {
			return committedProfile{}, false
		}
	}
	return loadCommitted(cfg)
}

func loadCommitted(cfg *config.Config) (committedProfile, bool) {
	if cfg == nil {
		return committedProfile{}, false
	}
	st, ok, err := routing.Load(cfg.StateDir, cfg.ProfileName)
	want := expectedAccount(cfg)
	if err != nil || !ok || want == "" || st.Account != want {
		return committedProfile{}, false
	}
	return committedProfile{cfg: cfg, st: st}, true
}

// committedRoutings returns every account-verified profile routing state: the active
// profile first, then any ActiveProfile override and every remaining profile declared in
// config.json in sorted order. Profiles with missing, unreadable, or account-mismatched
// routing states are skipped (fail closed).
func (d Deps) committedRoutings() []committedProfile {
	if d.LoadConfig == nil {
		return nil
	}
	active, err := d.LoadConfig()
	if err != nil || active == nil {
		return nil
	}
	var out []committedProfile
	seen := map[string]bool{}
	try := func(name string) {
		if name != "" {
			if seen[name] {
				return
			}
			seen[name] = true
		}
		if cp, ok := d.committedFor(name, active); ok {
			out = append(out, cp)
		}
	}
	try(active.ProfileName)
	if d.ActiveProfile != nil {
		if name := d.ActiveProfile(); name != "" {
			try(name)
		}
	}
	if active.Dir != "" {
		if f, err := config.LoadFile(active.Dir); err == nil {
			for _, name := range slices.Sorted(maps.Keys(f.Profiles)) {
				if name != "" {
					try(name)
				}
			}
		}
	}
	return out
}

// findCommitted locates enrollment's account-verified owning profile, checking hint first
// so per-enrollment lookups stay O(1) before falling back to scanning all profiles.
func (d Deps) findCommitted(enrollment, hint string) (committedProfile, routing.Team, bool) {
	if hint != "" && d.LoadConfig != nil {
		if active, err := d.LoadConfig(); err == nil && active != nil {
			if cp, ok := d.committedFor(hint, active); ok {
				if t, has := cp.st.Team(enrollment); has {
					return cp, t, true
				}
			}
		}
	}
	for _, cp := range d.committedRoutings() {
		if t, has := cp.st.Team(enrollment); has {
			return cp, t, true
		}
	}
	return committedProfile{}, routing.Team{}, false
}

func (d Deps) refreshTeam(ctx context.Context, cp committedProfile, t routing.Team, onChange func(...string)) error {
	cfg, st := cp.cfg, cp.st
	enrollment := t.EnrollmentID
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
