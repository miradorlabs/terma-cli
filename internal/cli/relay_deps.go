package cli

import (
	"context"
	"errors"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/globalmode"
	"github.com/miradorlabs/terma-cli/internal/policy"
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
		RelayAgents:   app.relayAgents,
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
		return app.globalMode().Apply(ctx, cfg.Harnesses, pol.Global(), func(string) {}, func(string) {})
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

func (app *App) relayAgents(dir, addr string) (pointed, blocked []string) {
	for _, e := range app.agents.With[agents.RelayExporter]() {
		if ok, known := e.RelayPointed(addr); known && ok {
			pointed = append(pointed, e.Name())
		}
		if c, ok := e.(agents.RelayChecker); ok {
			if _, _, problem := c.RelayProblem(dir); problem {
				blocked = append(blocked, e.Name())
			}
		}
	}
	return pointed, blocked
}

// relayHeartbeatSend delivers a heartbeat with the developer's credential; signed out,
// nothing is sent.
func (app *App) relayHeartbeatSend(ctx context.Context, beat *logspb.LogsData) error {
	cfg, err := config.Load(config.Overrides{})
	if err != nil {
		return err
	}
	if cfg.APIKey != "" {
		return errors.New("a server key is not a developer's sign-in: heartbeats go with one")
	}
	body, err := protojson.Marshal(beat)
	if err != nil {
		return err
	}
	client, err := app.newClient(cfg)
	if err != nil {
		return err
	}
	return client.SendHeartbeat(ctx, body)
}
