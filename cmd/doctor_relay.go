package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay"
)

// relayCheck is whether terma's relay can deliver what the agents pointed at it send:
// set up, running under its service manager, answering on loopback, and delivering.
// It applies only when an agent exports through it; both doctor and status show it.
func relayCheck(ctx context.Context, verdicts []harnessVerdict) doctor.Check {
	if !slices.ContainsFunc(verdicts, func(v harnessVerdict) bool { return v.route == routeRelay }) {
		return doctor.Check{Status: doctor.Skip, Detail: "no agent exports through it"}
	}
	const fix = "terma setup"
	rc, err := relayLoad()
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "agents export to the relay, and it is not set up (" + err.Error() + ")", Fix: fix}
	}
	svc, err := relayService(ctx)
	switch {
	case err != nil:
		return doctor.Check{Status: doctor.Fail, Detail: "could not ask the service manager about the relay: " + err.Error(), Fix: fix}
	case !svc.Installed:
		return doctor.Check{Status: doctor.Fail, Detail: "agents export to the relay, and its service is not installed — their telemetry is lost", Fix: fix}
	case !svc.Running:
		return doctor.Check{Status: doctor.Fail, Detail: "the relay's service is installed but not running — agents' telemetry is lost", Fix: fix}
	}
	h, err := relayProbe(ctx, rc)
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "the relay is not answering on " + rc.Endpoint() + ": " + err.Error(), Fix: fix}
	}
	detail := fmt.Sprintf("running on %s", rc.Endpoint())
	if h.Version != "" {
		detail += " (terma " + h.Version + ")"
	}
	if h.Backlog > 0 {
		detail += fmt.Sprintf(", %d request(s) waiting to be delivered", h.Backlog)
	}
	switch {
	case slices.Contains(h.HeldProjects, relay.MachineRoute):
		return doctor.Check{Status: doctor.Warn,
			Detail: detail + "; records for the machine project are held — none is chosen",
			Fix:    "terma setup"}
	case len(h.HeldProjects) > 0:
		return doctor.Check{Status: doctor.Warn,
			Detail: detail + "; held for a key: " + strings.Join(h.HeldProjects, ", "),
			Fix:    "terma install (in a repository bound to each held project, so this machine stores its key)"}
	case h.LastError != "":
		return doctor.Check{Status: doctor.Warn, Detail: detail + "; last delivery failed: " + h.LastError, Fix: "terma doctor (again once the network or the key is fixed)"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: detail}
}
