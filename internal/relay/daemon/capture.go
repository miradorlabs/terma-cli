package daemon

import (
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Capture is what decides a claimed session's content and signals.
type Capture struct {
	// Org is the collection policy for the claim's project: the ceiling.
	Org config.Policy
	// Primary is global mode's team default project, whose exports need no claim.
	Primary bool
	// Record is the developer's routing record, nil when none; RecordErr says it exists but could not be read.
	Record    *routing.Record
	RecordErr error
	// Harness is the claiming agent's harness name, empty for global mode's catch-all.
	Harness string
}

// CapturePolicy is the content and signal half of a claim's Policy. Content is the team
// policy's alone (config.Policy.Content, the rule hook events follow too); the routing
// record decides only signals, and withholds every one from an agent it does not name,
// since another repository may have pointed that agent's exporter at the relay.
func CapturePolicy(in Capture) relay.Policy {
	org := in.Org
	pol := relay.Policy{RequireClaim: !in.Primary || !org.Global()}
	pol.IncludePrompts, pol.IncludeToolContent = org.Content()
	switch rec := in.Record; {
	case org.CollectsNothing, in.RecordErr != nil,
		rec != nil && in.Harness != "" && !slices.Contains(rec.Harnesses, in.Harness):
		pol.IncludePrompts, pol.IncludeToolContent = false, false
		pol.Signals = []string{}
	case rec != nil:
		pol.Signals = append([]string{}, rec.Signals...)
	}
	return pol
}
