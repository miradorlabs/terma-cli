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
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
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
	var dir, token, addr string
	if err := app.withExporters(ctx, func() error {
		err := app.releaseDeselected(ctx, targets, r)
		if err != nil || len(targets) == 0 {
			return err
		}
		if dir, err = daemon.Dir(app.stateDir); err != nil {
			return err
		}
		if _, err = app.ensureRelayToken(); err != nil {
			return err
		}
		// One pair, never a moving relay's new token with its old address.
		if addr, token, err = daemon.Current(app.stateDir); err != nil {
			return err
		}
		return app.pointAgentsAtRelay(ctx, targets, addr, token, func(agent, detail string) {
			r.ok(agent, "sends to the local relay"+detail)
		}, r.then)
	}); err != nil || len(targets) == 0 {
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
	// A move gives the relay a new address and token; either changed, every agent follows.
	now, latest, err := daemon.Current(app.stateDir)
	if err == nil && (now != addr || latest != token) {
		if err := app.withExporters(ctx, func() error {
			return app.pointAgentsAtRelay(ctx, targets, now, latest, func(string, string) {}, r.then)
		}); err != nil {
			return err
		}
	}
	if m, ok := daemon.LastMove(dir); ok && m.To == now {
		daemon.Repointed(dir, now)
		if m.Recent(time.Now()) {
			r.warn("Relay", m.Describe())
		}
	}
	return nil
}

// relayMoved points the agents that export to from, an address the relay could not listen
// on, at to, where it listens now (daemon.Config.Moved). Only an exporter seen naming from
// is rewritten: the relay's environment may place an agent's settings elsewhere than the
// developer's does. `terma setup` points the rest, and doctor says so until it has.
func (app *App) relayMoved(ctx context.Context, from, to string, logf func(string, ...any)) {
	token, err := daemon.Token(app.stateDir)
	if err != nil {
		return
	}
	// Chosen under the lock as well: an exporter setup or teardown undoes meanwhile stays undone.
	err = app.withExporters(ctx, func() error {
		var targets []string
		for _, e := range app.agents.With[agents.RelayExporter]() {
			if pointed, _ := e.RelayPointed(from); pointed {
				targets = append(targets, e.Name())
			}
		}
		return app.pointAgentsAtRelay(ctx, targets, to, token, func(agent, detail string) {
			logf("relay: %s now exports to %s%s", agent, to, detail)
		}, func(note string) { logf("relay: %s", note) })
	})
	if err != nil {
		logf("relay: pointing the agents at %s: %v", to, err)
	}
}

// releaseDeselected undoes the relay exporter of every agent not in targets: global mode's
// catch-all would forward it. Callers hold withExporters.
func (app *App) releaseDeselected(ctx context.Context, targets []string, r relayReport) error {
	// An exporter the relay could not re-point when it moved holds a token the move replaced.
	tokens := daemon.Tokens(app.stateDir)
	for _, e := range app.agents.With[agents.RelayExporter]() {
		if slices.Contains(targets, e.Name()) {
			continue
		}
		var result agents.RelayResult
		for _, t := range tokens {
			undone, err := e.UnconfigureRelay(ctx, t)
			if err != nil {
				return fmt.Errorf("%s: stop sending to the relay: %w", e.DisplayName(), err)
			}
			result.Paths, result.Notes = append(result.Paths, undone.Paths...), append(result.Notes, undone.Notes...)
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
	if err := daemon.RecordAddr(dir, addr); err != nil {
		return err
	}
	daemon.Stop(dir)
	// The failure to listen on the old address would hold back the next start for a while.
	_ = os.Remove(filepath.Join(dir, daemon.ErrorFile))
	return nil
}
