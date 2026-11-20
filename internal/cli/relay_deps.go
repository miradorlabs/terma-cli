package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

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
		SendHeartbeat: app.relayHeartbeatSend,
		HookPolicy:    app.hookPolicy,
		LoadConfig:    app.loadConfig,
		RefreshPolicy: pols.Refresh,
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

// relayHeartbeatSend delivers a heartbeat to the selected team's ingest with the person's
// sign-in, naming the team; on a profile set up with a server key, with the selected
// team's key, else any project's held here. With neither, nothing is sent. It sends once,
// so the user teamUser names never rides on to another key.
func (app *App) relayHeartbeatSend(ctx context.Context, beat *logspb.LogsData) error {
	cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{})
	if err != nil {
		return err
	}
	selected := cmp.Or(cfg.Policy.TeamID, cfg.Policy.DefaultProjectID, cfg.ProjectID)
	// The profile decides, as for the relay, the hooks and delivery: TERMA_API_KEY is setup's
	// input, recorded as the profile's server-key sign-in.
	if !cfg.ServerKeySignIn {
		return app.signedInHeartbeat(ctx, cfg, selected, beat)
	}
	others := keystore.CollectionProjects(app.dir)
	slices.Sort(others)
	for _, projectID := range append([]string{selected}, others...) {
		key, err := keystore.Get(app.dir, projectID)
		if err != nil {
			return err // every key sits in the one keychain: asking for the next would wait as long
		}
		if projectID != "" && key != "" {
			endpoint := app.delivery().Endpoint(cfg, projectID)
			relay.NameHeartbeatUser(beat, app.teamUser(cfg, projectID, endpoint))
			return sendHeartbeat(ctx, endpoint, key, "", beat)
		}
	}
	return relay.ErrNoKey
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

// signedInHeartbeat sends beat to team's ingest with the person's sign-in, naming the team.
func (app *App) signedInHeartbeat(ctx context.Context, cfg *config.Config, team string, beat *logspb.LogsData) error {
	src := app.signInSource(cfg)
	account := src.Account()
	if team == "" || account == "" {
		return relay.ErrNoKey
	}
	token, err := src.Token(ctx, account)
	if err != nil {
		return err
	}
	if token == "" {
		return relay.ErrNoKey // signed out, retired, or another person's: no beat goes
	}
	endpoint := app.delivery().Endpoint(cfg, team)
	relay.NameHeartbeatUser(beat, app.teamUser(cfg, team, endpoint))
	err = sendHeartbeat(ctx, endpoint, token, team, beat)
	if refusal, ok := errors.AsType[*heartbeatError](err); ok {
		switch {
		case refusal.status == http.StatusUnauthorized:
			src.Refused(ctx, token)
		case refusal.reason == sendas.ReasonSignInRetired:
			src.Retired()
		}
	}
	return err
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
// cleartext.
var heartbeatClient = &http.Client{
	CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("refusing to follow redirect to %s: the OTLP endpoint does not redirect heartbeats", req.URL.Redacted())
	},
}
