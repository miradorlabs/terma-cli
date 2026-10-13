package cli

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"

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
// Only a beat that goes with the selected team's key names the signed-in user: any other key
// held here may belong to another organization or environment.
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
			if projectID == selected {
				relay.NameHeartbeatUser(beat, signedInUser(cfg))
			}
			return sendHeartbeat(ctx, app.delivery().Endpoint(cfg, projectID), key, beat)
		}
	}
	return relay.ErrNoKey
}

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
