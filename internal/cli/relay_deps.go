package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/globalmode"
	"github.com/miradorlabs/terma-cli/internal/policy"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// relayDeps are what the relay reaches through the command line.
func (app *App) relayDeps() daemon.Deps {
	pols := app.policies()
	return daemon.Deps{
		Version:       app.version,
		Correlators:   app.agents.With[shape.Correlator](),
		Capturers:     app.agents.With[shape.Capturer](),
		AgentName:     app.agents.NameForTool,
		RelayTargets:  app.agents.RelayTargets,
		Endpoint:      app.delivery().Endpoint,
		User:          app.relayUser,
		HookPolicy:    app.hookPolicy,
		LoadConfig:    app.loadConfig,
		RefreshPolicy: pols.Refresh,
		RefreshTeam:   pols.RefreshTeam,
		TeamUser:      app.selectedTeamUser,
		ActiveProfile: app.activeProfileName,
		LoadProfile:   app.loadProfileConfig,
		Bind:          app.bindRelay,
	}
}

// policies fetch collection policies as this build.
func (app *App) policies() policy.Source {
	return policy.Source{Version: app.version}
}

// globalMode is this machine as setup writes it.
func (app *App) globalMode() globalmode.Machine {
	return globalmode.Machine{ConfigDir: app.dir, Agents: app.agents, Terma: app.hookExecutable, ManagedRoot: app.managedRoot}
}

// relayHeartbeat is the relay's HeartbeatSend, sharing src with the relay's OTLP sender so
// heartbeats reuse its cached credential and in-memory retirement mark.
func (app *App) relayHeartbeat(src *signInSource) func(context.Context, *logspb.LogsData) error {
	return func(ctx context.Context, beat *logspb.LogsData) error {
		return app.relayHeartbeatSend(ctx, src, beat)
	}
}

// relayHeartbeatSend delivers a heartbeat to the selected team's ingest with the person's
// sign-in, naming the team; on a profile set up with a server key, with the selected
// team's key, else any project's held here. With neither, nothing is sent. It sends once,
// so the user teamUser names never rides on to another key.
func (app *App) relayHeartbeatSend(ctx context.Context, src *signInSource, beat *logspb.LogsData) error {
	overrides := app.configOverrides()
	if src != nil && src.cfg != nil && src.cfg.ProfileName != "" {
		overrides.Profile = src.cfg.ProfileName
	}
	var cfg *config.Config
	var loadErr error
	// Read the profile's mode, team and sign-in as one snapshot (readLaneAsOne): a switch
	// between config.json and user-credentials.json makes the lane unreadable, so a beat
	// never pairs one account's token with another's team.
	lane, _ := readLaneAsOne(app.dir, func() string {
		cfg, loadErr = config.Load(app.dir, app.stateDir, overrides)
		if loadErr != nil {
			return sendas.AccountUnreadable
		}
		return app.profileLane(cfg)
	})
	if loadErr != nil {
		return loadErr
	}
	return app.teamHeartbeatSend(ctx, src, cfg, lane, beat)
}

// relayUser is who the relay's parts sent to projectID at endpoint name: teamUser, under the
// relay's own profile (relay) as its configuration is now, since the developer can sign in,
// out or switch while the relay runs. It is asked as the relay accepts a part, as delivery
// asks as it flushes a hook event: who is signed in when terma hands on what it holds.
func (app *App) relayUser(relay *config.Config, projectID, endpoint string) string {
	cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{Profile: relay.ProfileName})
	if err != nil {
		return ""
	}
	return app.teamUser(cfg, projectID, endpoint)
}

// teamUser is who what terma sends with projectID's key to endpoint names (a heartbeat, a
// relayed part, a hook event), by Mirador user id: the signed-in user, only for the key of the
// team whose validated policy this login fetched, going to this profile's own ingest and filed,
// if with hosts at all, with this profile's API host. Any other key held here, TERMA_TEAM_ID's
// included, or one filed with another deployment's hosts may be another organization's or
// environment's, so what it sends names no one.
func (app *App) teamUser(cfg *config.Config, projectID, endpoint string) string {
	if projectID != cfg.Policy.Team() || !cfg.Policy.Validated() || !sameHost(endpoint, cfg.OTLPURL) {
		return ""
	}
	if h, ok := keystore.HostsFor(app.dir, projectID); ok && h.API != "" && !sameHost(h.API, cfg.APIURL) {
		return ""
	}
	return signedInUser(cfg)
}

// accountUser is teamUser when it is account's, the sign-in a token was had for ("" for a
// server key's): a person who signed in since, while it was had, is named on none of what
// it sends.
func (app *App) accountUser(cfg *config.Config, projectID, endpoint, account string) string {
	user := app.teamUser(cfg, projectID, endpoint)
	if auth.AccountKey(cfg.AuthURL, user) != account {
		return ""
	}
	return user
}

func sameHost(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// signedInUser is the Mirador user cfg's profile is signed in as, from
// user-credentials.json alone, never the keychain: "" when it is signed out, signed in for
// another environment, or set up with a server key, which is no one's.
func signedInUser(cfg *config.Config) string {
	if cfg.ServerKeySignIn {
		return ""
	}
	cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName)
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil {
		return ""
	}
	return cred.UserID
}

// heartbeatError is the ingest's refusal of a beat, with the reason a user token's carries.
type heartbeatError struct {
	status int
	reason string
}

func (e *heartbeatError) Error() string { return fmt.Sprintf("the ingest answered HTTP %d", e.status) }

// sendHeartbeat posts beat to endpoint with bearer, naming team for a user token ("" for a
// server key, whose grant fixes it).
func sendHeartbeat(ctx context.Context, endpoint, bearer, team string, beat *logspb.LogsData) error {
	body, err := proto.Marshal(beat)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/logs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if team != "" {
		req.Header.Set(relay.ProjectHeader, team)
	}
	resp, err := heartbeatClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &heartbeatError{status: resp.StatusCode, reason: sendas.Reason(body)}
	}
	return nil
}

// heartbeatClient refuses redirects, as the OTLP sender does: Go keeps the Authorization
// header on a same-host https→http downgrade, which would send the person's token in
// cleartext. Its Timeout bounds the request alone, as the relay's sendTimeout does a part's:
// the sign-in's token, had before it, may wait on a refresh well past it (#165).
var heartbeatClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("refusing to follow redirect to %s: the OTLP endpoint does not redirect heartbeats", req.URL.Redacted())
	},
}
