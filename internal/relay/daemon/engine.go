package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// PolicyRefreshInterval is how often each team's collection policy is fetched again, so a
// change made in the Terma web app reaches this machine within about half a minute.
const PolicyRefreshInterval = 30 * time.Second

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
	// CreateKey mints a project's key under cfg's sign-in.
	CreateKey func(ctx context.Context, cfg *config.Config, projectID string) (string, error)
	// SendHeartbeat delivers a heartbeat with a project's key.
	SendHeartbeat func(ctx context.Context, beat *logspb.LogsData) error
	// HookPolicy is the organization's policy as a hook reads it.
	HookPolicy func() config.Policy
	// LoadConfig is the configuration as the command line resolves it.
	LoadConfig func() (*config.Config, error)
	// RefreshPolicy fetches and stores cfg's project's policy.
	RefreshPolicy func(ctx context.Context, cfg *config.Config) error
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

// Prepare makes cfg the configuration a relay routes with: the signed-in organization,
// and no capture until the first successful policy fetch.
func Prepare(cfg *config.Config) {
	if cfg.OrganizationID == "" && config.PolicyStub() == "" {
		if cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName); err == nil && cred.CheckEnvironment(cfg.AuthURL) == nil {
			cfg.OrganizationID = cred.OrganizationID
		}
	}
	if !cfg.Policy.Validated() {
		cfg.Policy = config.NoPolicy(cfg.OrganizationID, cfg.AuthURL)
	}
}

// Engine is the relay's options for this machine. It starts nothing: the minter works
// only when a claim asks for a key, until ctx ends.
func (d Deps) Engine(ctx context.Context, stateDir string, cfg *config.Config, s Settings, logw io.Writer) relay.Options {
	minter := NewKeyMinter(ctx, cfg, d.CreateKey)
	opts := relay.Options{Correlators: d.Correlators, Capturers: d.Capturers,
		Hold: s.Hold, Dir: filepath.Join(claim.Dir(stateDir), relay.OutboxDir), Resolve: d.Resolver(cfg, minter.Mint), Version: d.Version,
		Lookup:   func(sessionID string, now time.Time) (claim.Claim, bool) { return claim.Read(stateDir, sessionID, now) },
		CatchAll: d.CatchAll(), HeartbeatSend: d.SendHeartbeat, HeartbeatEvery: s.Heartbeat,
		PeerPID: procinfo.FindSender, ProcessAlive: procinfo.Alive, ClaimCacheTTL: time.Second, PolicyCacheTTL: time.Second}
	if s.Debug && logw != nil {
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(logw, time.Now().Format("15:04:05.000 ")+f+"\n", a...) }
	}
	return opts
}

// Resolver turns a claim into its session's policy, minting a missing key with mint.
func (d Deps) Resolver(cfg *config.Config, mint func(projectID string)) func(claim.Claim) (relay.Policy, error) {
	return Resolver(cfg, ResolverDeps{Mint: mint, AgentName: d.AgentName, RelayTargets: d.RelayTargets,
		Endpoint: func(projectID string) string { return d.Endpoint(cfg, projectID) }})
}

// CatchAll is global mode's claim for everything an agent exports.
func (d Deps) CatchAll() func() (claim.Claim, bool) { return CatchAll(d.HookPolicy) }

// Refresher keeps fresh, while the relay runs, every team with a key here and the
// selected team.
func (d Deps) Refresher() *PolicyRefresher {
	return &PolicyRefresher{
		Interval: PolicyRefreshInterval,
		Discover: 5 * time.Second,
		Teams: func() []string {
			cfg, err := d.LoadConfig()
			if err != nil {
				return nil
			}
			teams := keystore.CollectionProjects(cfg.Dir)
			selected := cfg.Policy.Team()
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
