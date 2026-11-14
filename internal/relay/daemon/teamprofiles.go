package daemon

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// The team router keeps every profile's routing apart: a record goes by the profile its
// placement names, an enrollment by the profile that selects it.

// profileRoute is a profile's routing as the relay read it: its configuration, its
// committed routing state and the router compiled from it; ok is false while the state is
// missing, unreadable or not yet committed.
type profileRoute struct {
	cfg      *config.Config
	st       routing.State
	router   *routing.Router
	ok       bool
	loadedAt time.Time
}

// activeProfile is the profile hooks route by now (TeamsDeps.Active), asked at most once
// a second.
func (t *Teams) activeProfile() string {
	now := time.Now()
	t.mu.Lock()
	if t.active != "" && now.Sub(t.activeAt) < t.ttl {
		defer t.mu.Unlock()
		return t.active
	}
	t.mu.Unlock()
	name := t.cfg.ProfileName
	if t.deps.Active != nil {
		name = cmp.Or(t.deps.Active(), name)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active, t.activeAt = name, now
	return name
}

// route is profile's routing, the relay's start profile's for "" (a record whose placement
// names none, a part queued before the migration): its committed state, reread at most once
// a second, with its configuration; the router is compiled again only for a new generation.
// The active profile may change while the relay runs, and never moves a record that does
// not name it.
func (t *Teams) route(profile string) profileRoute {
	if profile == "" {
		profile = t.cfg.ProfileName
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	pr := t.profiles[profile]
	if pr == nil {
		pr = &profileRoute{}
		t.profiles[profile] = pr
	}
	if !pr.loadedAt.IsZero() && now.Sub(pr.loadedAt) < t.ttl {
		return *pr
	}
	pr.loadedAt = now
	// A crash in the middle of migrating collects nothing until the next start commits.
	st, found, err := routing.LoadReady(t.cfg.StateDir, profile)
	cfg := t.profileConfig(profile)
	switch {
	case err != nil || !found || cfg == nil:
		pr.ok = false
	default:
		if !pr.ok || pr.router == nil || !routing.SameGeneration(st, pr.st) {
			pr.router = routing.NewRouter(st)
		}
		pr.st, pr.cfg, pr.ok = st, cfg, true
	}
	return *pr
}

// profileConfig is name's configuration (TeamsDeps.Profile); nil when it cannot be read.
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

// owner is the routing of the profile that selects enrollment: where it was found last,
// else the active profile, then every other profile config.json names, then any other
// the relay has read. The last is tried before config.json is read, which a queue's every
// recheck would otherwise pay for. waits is set when it was found nowhere and a state that
// might select it cannot be read now.
func (t *Teams) owner(enrollment string) (pr profileRoute, found, waits bool) {
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
			if pr = t.route(name); !pr.ok {
				waits = true
				continue
			}
			if pr.st.Team(enrollment) != nil {
				t.mu.Lock()
				t.owners[enrollment] = name
				t.mu.Unlock()
				return true
			}
		}
		return false
	}
	if try(last, t.activeProfile()) {
		return pr, true, false
	}
	if f, err := config.LoadFile(t.cfg.Dir); err == nil && try(slices.Sorted(maps.Keys(f.Profiles))...) {
		return pr, true, false
	}
	if try(slices.Sorted(slices.Values(read))...) {
		return pr, true, false
	}
	return profileRoute{}, false, waits
}
