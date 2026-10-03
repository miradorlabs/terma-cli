package daemon

import (
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
)

// Capture is what decides a claimed session's content and signals.
type Capture struct {
	// Org is the collection policy for the claim's project: the ceiling.
	Org config.Policy
	// Primary is global mode's team default project, whose exports need no claim.
	Primary bool
	// Agents are the developer's agents that send through the relay.
	Agents []string
	// Harness is the claiming agent's harness name, empty for global mode's catch-all.
	Harness string
	// Repository is where the claim's session runs.
	Repository config.Repository
}

// CapturePolicy is the content and signal half of a claim's Policy, and whether the team
// policy still admits the claim's repository. Content is the team policy's alone (config.Policy.Content, the rule hook events follow too), and every
// signal is sent, except from an agent the developer did not choose, whose exporter an
// earlier setup may have left pointing at the relay.
func CapturePolicy(in Capture) relay.Policy {
	org := in.Org
	pol := relay.Policy{RequireClaim: !in.Primary || !org.Global(), Unadmitted: !org.Admits(in.Repository)}
	pol.IncludePrompts, pol.IncludeToolContent = org.Content()
	if org.CollectsNothing || !org.Global() && in.Harness != "" && !slices.Contains(in.Agents, in.Harness) {
		pol.IncludePrompts, pol.IncludeToolContent = false, false
		pol.Signals = []string{}
	}
	return pol
}
