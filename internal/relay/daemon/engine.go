package daemon

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// PolicyRefreshInterval is how often each team's collection policy is fetched again.
const PolicyRefreshInterval = time.Minute

// Deps are what the relay reaches through the command line: the agents' declarations and
// the network.
type Deps struct {
	Version     string
	Correlators []shape.Correlator
	Capturers   []shape.Capturer
	// AgentName is the agent behind a tool label.
	AgentName func(tool string) string
	// RelayTargets names the agents among the developer's choices that send through the relay.
	RelayTargets func(selected []string) []string
	// Endpoint is a project's ingest host.
	Endpoint func(cfg *config.Config, projectID string) string
	// User is who parts sent to projectID at endpoint name under cfg's profile, by Mirador user id.
	User func(cfg *config.Config, projectID, endpoint string) string
	// HookPolicy is the organization's policy as a hook reads it.
	HookPolicy func() config.Policy
	// LoadConfig is the configuration as the command line resolves it.
	LoadConfig func() (*config.Config, error)
	// RefreshPolicy fetches and stores cfg's project's policy.
	RefreshPolicy func(ctx context.Context, cfg *config.Config) error
	// RefreshTeam fetches a routed enrollment's policy without mutating disk state.
	RefreshTeam func(ctx context.Context, cfg *config.Config, account string, t routing.Team) (routing.Refresh, error)
	// TeamUser returns the Mirador user ID for a routed team at endpoint ("" on server keys).
	TeamUser func(cfg *config.Config, team routing.Team, endpoint string) string
	// ActiveProfile returns the profile hooks currently route by.
	ActiveProfile func() string
	// LoadProfile loads the named profile's configuration.
	LoadProfile func(name string) (*config.Config, error)
	// Bind gives the relay cfg's sign-in to send and heartbeat with, and what it binds
	// queued parts to; nil sends only with a server key and sends no heartbeats.
	Bind func(opts *relay.Options, cfg *config.Config)
}

// Settings are the relay's timings: TERMA_RELAY_HOLD and TERMA_RELAY_HEARTBEAT shorten
// them for tests, and TERMA_RELAY_DEBUG=1 logs every drop.
type Settings struct {
	Hold, Heartbeat time.Duration
	Debug           bool
}

// SettingsFromEnv reads Settings from the environment.
func SettingsFromEnv() Settings {
	s := Settings{Hold: relay.DefaultHold, Debug: os.Getenv("TERMA_RELAY_DEBUG") == "1"}
	if v, err := time.ParseDuration(os.Getenv("TERMA_RELAY_HOLD")); err == nil && v > 0 {
		s.Hold = v
	}
	if v, err := time.ParseDuration(os.Getenv("TERMA_RELAY_HEARTBEAT")); err == nil && v > 0 {
		s.Heartbeat = v
	}
	return s
}

// Prepare makes cfg the configuration a relay routes with: no capture until the first
// successful policy fetch.
func Prepare(cfg *config.Config) {
	if !cfg.Policy.Validated() {
		cfg.Policy = config.NoPolicy(cfg.OrganizationID, cfg.AuthURL)
	}
}

// Engine is the relay's options for this machine. It starts nothing.
func (d Deps) Engine(ctx context.Context, stateDir string, cfg *config.Config, s Settings, logw io.Writer) relay.Options {
	opts := relay.Options{Correlators: d.Correlators, Capturers: d.Capturers,
		Hold: s.Hold, Dir: filepath.Join(claim.Dir(stateDir), relay.OutboxDir), Resolve: d.Resolver(cfg), Version: d.Version,
		Lookup:   func(sessionID string, now time.Time) (claim.Claim, bool) { return claim.Read(stateDir, sessionID, now) },
		CatchAll: d.CatchAll(), HeartbeatEvery: s.Heartbeat,
		PeerPID: procinfo.FindSender, ProcessAlive: procinfo.Alive, ClaimCacheTTL: time.Second, PolicyCacheTTL: time.Second,
		HomeDir: homeDir()}
	if d.Bind != nil {
		d.Bind(&opts, cfg)
	}
	if s.Debug && logw != nil {
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(logw, time.Now().Format("15:04:05.000 ")+f+"\n", a...) }
	}
	return opts
}

// homeDir is the home directory of the user the relay runs as, read once as it starts: $HOME,
// as the agents it serves read it, symlink-resolved, absolute and with no trailing slash; ""
// when it cannot be read, names no absolute path, or is the filesystem's root, which is all
// slash.
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if home = filepath.Clean(home); !filepath.IsAbs(home) || home == filepath.VolumeName(home)+string(filepath.Separator) {
		return ""
	}
	return home
}

// Teams returns the multi-org relay.Router for cfg's profile.
func (d Deps) Teams(cfg *config.Config) *Teams {
	return NewTeams(cfg, TeamsDeps{
		AgentName: d.AgentName, RelayTargets: d.RelayTargets, Endpoint: d.Endpoint,
		User: d.TeamUser, Active: d.ActiveProfile, Profile: d.LoadProfile,
	})
}

// Resolver turns a claim into its session's policy.
func (d Deps) Resolver(cfg *config.Config) func(claim.Claim) (relay.Policy, error) {
	return Resolver(cfg, ResolverDeps{AgentName: d.AgentName, RelayTargets: d.RelayTargets,
		Endpoint: func(projectID string, serverKey bool) string {
			// The profile's mode as it is now, not at startup: a person's token never goes to
			// a host keys.json filed for a server key.
			live := *cfg
			live.ServerKeySignIn = serverKey
			return d.Endpoint(&live, projectID)
		},
		User: func(projectID, endpoint string) string { return d.User(cfg, projectID, endpoint) }})
}

// CatchAll is global mode's claim for everything an agent exports.
func (d Deps) CatchAll() func() (claim.Claim, bool) { return CatchAll(d.HookPolicy) }

// Refresher keeps fresh, while the relay runs, every team with a key here and the
// selected team. On a routed profile (routing/<profile>.json exists) it yields to
// TeamRefresher so legacy StorePolicy never writes config.json.
func (d Deps) Refresher() *PolicyRefresher {
	return &PolicyRefresher{
		Interval: PolicyRefreshInterval,
		Discover: 5 * time.Second,
		Teams: func() []string {
			cfg, err := d.LoadConfig()
			if err != nil {
				return nil
			}
			if cfg.StateDir != "" && cfg.ProfileName != "" {
				if _, found, rerr := routing.Load(cfg.StateDir, cfg.ProfileName); found || rerr != nil {
					return nil
				}
			}
			teams := keystore.CollectionProjects(cfg.Dir)
			// The selected team even while its policy isn't stored (a fresh or cleared state
			// directory): a person's sign-in holds no team key to name it otherwise.
			selected := cmp.Or(cfg.Policy.Team(), cfg.Team)
			if selected != "" && !slices.Contains(teams, selected) {
				teams = append(teams, selected)
			}
			return teams
		},
		Fetched: func(team string) time.Time {
			cfg, err := d.LoadConfig()
			if err != nil {
				return time.Time{}
			}
			if cached, ok := routing.ValidatedPolicy(cfg, team); ok {
				return cached.FetchedAt
			}
			return time.Time{}
		},
		Refresh: func(ctx context.Context, team string) error {
			cfg, err := d.LoadConfig()
			if err != nil {
				return err
			}
			scoped := *cfg
			scoped.ProjectID = team
			return d.RefreshPolicy(ctx, &scoped)
		},
	}
}
