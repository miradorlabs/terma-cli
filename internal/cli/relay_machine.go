package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
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
	if len(targets) == 0 {
		return nil
	}
	dir, err := daemon.Dir()
	if err != nil {
		return err
	}
	token, err := ensureRelayToken()
	if err != nil {
		return err
	}
	addr := daemon.Addr(dir)
	fmt.Fprintln(r.detail, "\nPointing agents at the local relay on "+addr+":")
	err = app.pointAgentsAtRelay(ctx, targets, addr, token, func(agent, detail string) {
		fmt.Fprintf(r.detail, "  %s%s\n", agent, detail)
		r.ok(agent, "exports through the local relay; only opted-in sessions leave")
	}, r.then)
	if err != nil {
		return err
	}
	ensureRelay(ctx, relayService, func(warn bool, what string) {
		if warn {
			r.warn("Relay", what)
		} else {
			r.ok("Relay", what)
		}
	})
	return nil
}
