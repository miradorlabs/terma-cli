package cmd

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

// relayHeartbeat is the heartbeat's facts about this terma and its agents.
func relayHeartbeat(dir string) daemon.Heartbeat {
	return daemon.Heartbeat{Dir: dir, Version: Version, InstallKind: installKind(),
		Agents: func(addr string) (pointed, blocked []string) {
			for _, e := range registered.With[agents.RelayExporter]() {
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

// installKind is how this terma was installed: the package manager that owns it, a
// source build, or the install script's.
func installKind() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	if m, ok := selfupdate.ManagedBy(exe); ok {
		return strings.ToLower(m.Name)
	}
	if !strings.HasPrefix(Version, "v") || strings.Contains(Version, "-g") || Version == "dev" {
		return "source"
	}
	return "script"
}

// relayHeartbeatSend delivers a heartbeat to the organization the developer signed in
// to, with their credential (api.SendHeartbeat). Not signed in, there is no organization
// to tell, and nothing is sent.
func relayHeartbeatSend(ctx context.Context, beat *logspb.LogsData) error {
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
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	return client.SendHeartbeat(ctx, body)
}
