package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// relayReport is how the machine-level relay setup reports to setup or install: ok and
// warn per step, then for a developer's step, detail for the long form.
type relayReport struct {
	ok     func(label, what string)
	warn   func(label, what string)
	then   func(step string)
	detail io.Writer
}

// connectMachineRelay is the machine half of the relay: the token, each agent's user-level
// exporter pointed at it, and the relay itself; claims decide per repository what leaves.
func (app *App) connectMachineRelay(ctx context.Context, agents []string, relayService string, r relayReport) error {
	targets := app.agents.RelayTargets(agents)
	if err := app.releaseDeselected(ctx, targets, r); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	dir, err := daemon.Dir(app.stateDir)
	if err != nil {
		return err
	}
	token, err := app.ensureRelayToken()
	if err != nil {
		return err
	}
	addr := daemon.Addr(dir)
	err = app.pointAgentsAtRelay(ctx, targets, addr, token, func(agent, detail string) {
		r.ok(agent, "sends to the local relay"+detail)
	}, r.then)
	if err != nil {
		return err
	}
	env := ""
	if cfg, err := app.loadConfig(); err == nil {
		env = cfg.Environment
	}
	app.ensureRelay(ctx, relayService, env, func(warn bool, what string) {
		if warn {
			r.warn("Relay", what)
		} else {
			r.ok("Relay", what)
		}
	})
	return nil
}

// releaseDeselected undoes the relay exporter of every agent not in targets: global mode's catch-all would forward it.
func (app *App) releaseDeselected(ctx context.Context, targets []string, r relayReport) error {
	token, _ := daemon.Token(app.stateDir)
	for _, e := range app.agents.With[agents.RelayExporter]() {
		if slices.Contains(targets, e.Name()) {
			continue
		}
		result, err := e.UnconfigureRelay(ctx, token)
		if err != nil {
			return fmt.Errorf("%s: stop sending to the relay: %w", e.DisplayName(), err)
		}
		if len(result.Paths) > 0 {
			paths := make([]string, 0, len(result.Paths))
			for _, path := range result.Paths {
				paths = append(paths, output.TildePath(path))
			}
			r.ok(e.DisplayName(), "no longer sends to the local relay ("+strings.Join(paths, ", ")+")")
		}
		for _, note := range result.Notes {
			r.then(note)
		}
	}
	return nil
}

// checkRelayAddr accepts a loopback host:port: the agents send to it and the relay takes
// telemetry from local senders only.
func checkRelayAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--relay-addr %q: want host:port (%w)", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--relay-addr %q: the relay listens on a loopback address, such as 127.0.0.1:4319", addr)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("--relay-addr %q: want a port from 1 to 65535", addr)
	}
	return nil
}

// moveRelay records addr as where the relay listens and stops one running elsewhere; the
// relay step that follows points the agents there and starts it again.
func (app *App) moveRelay(addr string) error {
	if err := checkRelayAddr(addr); err != nil {
		return err
	}
	dir, err := daemon.Dir(app.stateDir)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, daemon.AddrFile), []byte(addr+"\n"), 0o600); err != nil {
		return err
	}
	daemon.Stop(dir)
	// The failure to listen on the old address would hold back the next start for a while.
	_ = os.Remove(filepath.Join(dir, daemon.ErrorFile))
	return nil
}
