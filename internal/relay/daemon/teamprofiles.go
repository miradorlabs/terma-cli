package daemon

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// profileRoute caches one profile's loaded routing state and compiled router. exists is
// true whenever routing/<profile>.json exists on disk (even if corrupt or belonging to
// another account); ok is true only when it loaded cleanly and its State.Account matches
// the profile's expected lane.
type profileRoute struct {
	cfg      *config.Config
	st       routing.State
	router   *routing.Router
	exists   bool
	ok       bool
	loadedAt time.Time
}

// expectedAccount returns the routing.State.Account lane expected for cfg:
// sendas.NoPersonLane on a server-key profile, else cfg.Account.
func expectedAccount(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.ServerKeySignIn {
		return sendas.NoPersonLane
	}
	return cfg.Account
}

// Active reports whether routing is active for the relay's profile (routing/<profile>.json
// exists on disk, even if corrupt or mismatched so it fails closed rather than falling
// back to the pre-cutover single-team path).
func (t *Teams) Active() bool { return t.route("", false).exists }

// Invalidate clears cached routing-state snapshots so the next lookup reloads from disk.
func (t *Teams) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.activeAt = time.Time{}
	for _, pr := range t.profiles {
		pr.loadedAt = time.Time{}
	}
}

func (t *Teams) activeProfile(fresh bool) string {
	now := t.deps.Now()
	if !fresh {
		t.mu.Lock()
		if t.active != "" && now.Sub(t.activeAt) < t.ttl {
			defer t.mu.Unlock()
			return t.active
		}
		t.mu.Unlock()
	}
	name := t.cfg.ProfileName
	if t.deps.Active != nil {
		name = cmp.Or(t.deps.Active(), name)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active, t.activeAt = name, now
	return name
}

func (t *Teams) route(profile string, fresh bool) profileRoute {
	if profile == "" {
		profile = t.cfg.ProfileName
	}
	now := t.deps.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	pr := t.profiles[profile]
	if pr == nil {
		pr = &profileRoute{}
		t.profiles[profile] = pr
	}
	if !fresh && !pr.loadedAt.IsZero() && now.Sub(pr.loadedAt) < t.ttl {
		return *pr
	}
	pr.loadedAt = now
	if t.cfg.StateDir == "" || profile == "" {
		pr.st, pr.cfg, pr.router, pr.exists, pr.ok = routing.State{}, nil, nil, false, false
		return *pr
	}
	st, found, err := routing.Load(t.cfg.StateDir, profile)
	pr.exists = found || err != nil
	cfg := t.profileConfig(profile)
	want := expectedAccount(cfg)
	switch {
	case err != nil || !found || cfg == nil || want == "" || st.Account != want:
		pr.st, pr.cfg, pr.router, pr.ok = routing.State{}, cfg, nil, false
	default:
		if !pr.ok || pr.router == nil || !routing.SameGeneration(st, pr.st) {
			pr.router = routing.NewRouter(st)
		}
		pr.st, pr.cfg, pr.ok = st, cfg, true
	}
	return *pr
}

func (t *Teams) profileConfig(name string) *config.Config {
	if t.deps.Profile == nil {
		c := *t.cfg
		c.ProfileName = name
		return &c
	}
	c, err := t.deps.Profile(name)
	if err != nil {
		return nil
	}
	return c
}

// owner finds the profile that selects enrollment for account. waits is true when
// enrollment was not found and some profile's routing state could not be read, failed
// account verification, or currently belongs to another account — preventing transient
// errors or another user's state from falsely discarding queued telemetry.
func (t *Teams) owner(enrollment, account string, fresh bool) (pr profileRoute, found, waits bool) {
	t.mu.Lock()
	last := t.owners[enrollment]
	read := slices.Collect(maps.Keys(t.profiles))
	t.mu.Unlock()
	tried := map[string]bool{}
	try := func(names ...string) bool {
		for _, name := range names {
			if name == "" || tried[name] {
				continue
			}
			tried[name] = true
			pr = t.route(name, fresh)
			if !pr.ok {
				waits = true
				continue
			}
			if account != "" && pr.st.Account != account {
				waits = true
				continue
			}
			if _, has := pr.st.Team(enrollment); has {
				t.mu.Lock()
				t.owners[enrollment] = name
				t.mu.Unlock()
				return true
			}
		}
		return false
	}
	if try(last, t.activeProfile(fresh)) {
		return pr, true, false
	}
	if f, err := config.LoadFile(t.cfg.Dir); err != nil {
		waits = true
	} else if try(slices.Sorted(maps.Keys(f.Profiles))...) {
		return pr, true, false
	}
	if try(slices.Sorted(slices.Values(read))...) {
		return pr, true, false
	}
	return profileRoute{}, false, waits
}
