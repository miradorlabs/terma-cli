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
		Endpoint:      app.delivery().Endpoint,
		CreateKey:     app.createProjectKey,
		SendHeartbeat: app.relayHeartbeatSend,
		HookPolicy:    hookPolicy,
		LoadConfig:    app.loadConfig,
		RefreshPolicy: pols.Refresh,
	}
}

// policies fetch collection policies as this build, and switch global mode when the
// profile's own team's changes.
func (app *App) policies() policy.Source {
	return policy.Source{Version: app.version, ModeChanged: func(ctx context.Context, cfg *config.Config, pol config.Policy) error {
		return app.globalMode().Apply(ctx, cfg.Harnesses, pol.Global(), func(string) {}, func(string) {}, func(string) {})
	}}
}

// globalMode is this machine as global mode writes it.
func (app *App) globalMode() globalmode.Machine {
	return globalmode.Machine{Agents: app.agents, Terma: app.hookExecutable, ManagedRoot: app.managedRoot, RelayDir: daemon.Dir}
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
func (app *App) relayHeartbeatSend(ctx context.Context, beat *logspb.LogsData) error {
	cfg, err := config.Load(config.Overrides{})
	if err != nil {
		return err
	}
	selected := cmp.Or(cfg.Policy.TeamID, cfg.Policy.DefaultProjectID, cfg.ProjectID)
	if selected != "" && keystore.Get(selected) == "" && cfg.APIKey == "" {
		if key, err := app.createProjectKey(ctx, cfg, selected); err == nil && key != "" {
			_ = keystore.Set(selected, key, keystore.HostsOf(cfg))
		}
	}
	others := keystore.CollectionProjects()
	slices.Sort(others)
	for _, projectID := range append([]string{selected}, others...) {
		if key := keystore.Get(projectID); projectID != "" && key != "" {
			return sendHeartbeat(ctx, app.delivery().Endpoint(cfg, projectID), key, beat)
		}
	}
	return relay.ErrNoKey
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
