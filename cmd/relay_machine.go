package cmd

import (
	"context"
	"fmt"
	"io"
)

// relayReport is how the machine-level relay setup tells its caller what it did: ok
// and warn per step (label, what), then for a step the developer must take, and
// detail for the long form. `terma setup` and `terma install` each render it their way.
type relayReport struct {
	ok     func(label, what string)
	warn   func(label, what string)
	then   func(step string)
	detail io.Writer
}

// connectMachineRelay is the machine half of the relay, done once per machine by
// `terma setup` (and by `terma install` when setup has not): the local token, each of
// the developer's agents' user-level exporters pointed at the relay, and the relay
// itself, as a service unless the developer opted out. It needs no project: which
// sessions leave is the claims' business, per repository.
func connectMachineRelay(ctx context.Context, agents []string, relayService string, r relayReport) error {
	targets := registered.RelayTargets(agents)
	if len(targets) == 0 {
		return nil
	}
	dir, err := relayDir()
	if err != nil {
		return err
	}
	token, err := ensureRelayToken()
	if err != nil {
		return err
	}
	addr := relayAddr(dir)
	fmt.Fprintln(r.detail, "\nPointing agents at the local relay on "+addr+":")
	err = pointAgentsAtRelay(ctx, targets, addr, token, func(agent, detail string) {
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
