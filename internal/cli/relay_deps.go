package cli

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
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
		CreateKey:     app.createProjectKey,
		SendHeartbeat: app.relayHeartbeatSend,
		HookPolicy:    app.hookPolicy,
		LoadConfig:    app.loadConfig,
		RefreshPolicy: pols.Refresh,
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

func (app *App) createProjectKey(ctx context.Context, cfg *config.Config, projectID string) (string, error) {
	scoped := *cfg
	scoped.ProjectID = projectID
	client, err := app.newClient(&scoped)
	if err != nil {
		return "", err
	}
	key, _, err := client.CreateServerKey(ctx, projectID, "terma-relay@"+hostname(),
		"Created by the terma relay, for a repository connected in Terma")
	return key, err
}

// relayHeartbeatSend delivers a heartbeat to a project's ingest with that project's key: the
// selected team's, minted if missing, else any project's held here; with none, nothing is sent.
// It sends once, so the user teamUser names never rides on to another key.
func (app *App) relayHeartbeatSend(ctx context.Context, beat *logspb.LogsData) error {
	cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{})
	if err != nil {
		return err
	}
	selected := cmp.Or(cfg.Policy.TeamID, cfg.Policy.DefaultProjectID, cfg.ProjectID)
	if key, err := keystore.Get(app.dir, selected); selected != "" && key == "" && err == nil && cfg.APIKey == "" {
		if key, err := app.createProjectKey(ctx, cfg, selected); err == nil && key != "" {
			_ = keystore.Set(app.dir, selected, key, keystore.HostsOf(cfg))
		}
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
			return sendHeartbeat(ctx, endpoint, key, beat)
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

// signedInUser is the Mirador user cfg's profile is signed in as, from credentials.json alone,
// never the keychain: "" when it is signed out, signed in for another organization or
// environment, or set up with a server key, which is no one's.
func signedInUser(cfg *config.Config) string {
	if cfg.ServerKeySignIn {
		return ""
	}
	cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName)
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil || cred.OrganizationID != cfg.OrganizationID {
		return ""
	}
	return cred.UserID
}

func sendHeartbeat(ctx context.Context, endpoint, key string, beat *logspb.LogsData) error {
	body, err := proto.Marshal(beat)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/logs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the ingest answered HTTP %d", resp.StatusCode)
	}
	return nil
}
