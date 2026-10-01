package cli

import (
	"context"
	"errors"
	"os"
	"strings"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

func (app *App) relayHeartbeat(dir string) daemon.Heartbeat {
	return daemon.Heartbeat{Dir: dir, Version: app.version, InstallKind: app.installKind(),
		Agents: func(addr string) (pointed, blocked []string) {
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
		}}
}

// installKind is the package manager that owns this terma, "source", or "script".
func (app *App) installKind() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	if m, ok := selfupdate.ManagedBy(exe); ok {
		return strings.ToLower(m.Name)
	}
	if !strings.HasPrefix(app.version, "v") || strings.Contains(app.version, "-g") || app.version == "dev" {
		return "source"
	}
	return "script"
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
