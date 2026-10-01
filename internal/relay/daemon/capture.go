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

// CapturePolicy is the content and signal half of a claim's Policy, with the
// organization's policy as the ceiling that the routing record can only narrow.
// Path exclusions withhold all free text, since exporters do not name its source files,
// and a record withholds an agent it does not name: another repository may have pointed
// that agent's exporter at the relay machine-wide.
func CapturePolicy(in Capture) relay.Policy {
	org := in.Org
	pol := relay.Policy{IncludePrompts: org.IncludePrompts, IncludeToolContent: org.IncludeToolContent, Signals: org.Signals,
		Excludes: excludes(org.ExcludePaths), RequireClaim: !in.Primary || !org.Global()}
	if len(org.ExcludePaths) > 0 {
		pol.IncludePrompts, pol.IncludeToolContent = false, false
	}
	switch rec := in.Record; {
	case in.RecordErr != nil:
		pol.IncludePrompts, pol.IncludeToolContent = false, false
		pol.Signals = []string{}
	case rec == nil:
	case in.Harness != "" && !slices.Contains(rec.Harnesses, in.Harness):
		pol.IncludePrompts, pol.IncludeToolContent = false, false
		pol.Signals = []string{}
	default:
		pol.IncludePrompts = pol.IncludePrompts && rec.IncludePrompts
		pol.IncludeToolContent = pol.IncludeToolContent && rec.IncludeToolContent
		pol.Signals = []string{}
		for _, s := range rec.Signals {
			if org.AllowsSignal(s) {
				pol.Signals = append(pol.Signals, s)
			}
		}
	}
	return pol
}

// excludes matches values naming one of patterns, nil when there are none. It keeps its
// own copy, so a policy refreshed later never changes a decision already made.
func excludes(patterns []string) func(any) bool {
	if len(patterns) == 0 {
		return nil
	}
	p := config.Policy{ExcludePaths: slices.Clone(patterns)}
	return func(v any) bool { return p.HasExcludedPath(v, "") }
}
