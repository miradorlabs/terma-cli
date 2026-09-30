package relay

import (
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Capture is what decides a claimed session's content and signals.
type Capture struct {
	// Org is the collection policy that applies to the claim's project
	// (routing.EffectivePolicy): the ceiling.
	Org config.Policy
	// Primary is global mode's own project: the team's default, whose exports need
	// no claim.
	Primary bool
	// Record is the developer's routing record for the project, nil when there is
	// none; RecordErr is a record that exists and could not be read.
	Record    *routing.Record
	RecordErr error
	// Harness is the claiming agent's harness name (exporter.NameForTool), empty for
	// global mode's catch-all, which has no tool.
	Harness string
}

// CapturePolicy is the content and signal half of a claim's Policy; the caller adds
// the endpoint and key. The organization's policy is the ceiling, and the routing
// record can only narrow it:
//
//   - Path exclusions withhold prompts and tool content: native exporters do not
//     name the source files of arbitrary prompt, response and tool text, so none of
//     it can be shown to be safe. Named excluded paths are filtered separately.
//   - A record that exists and cannot be read withholds everything.
//   - A record withholds a claimed session of an agent it does not name: another
//     repository may have pointed that agent's exporter at the relay machine-wide.
//   - Otherwise prompts and tool content need both the policy and the record, and a
//     signal leaves only when both allow it.
//
// No record leaves the organization's policy as it is.
func CapturePolicy(in Capture) Policy {
	org := in.Org
	pol := Policy{IncludePrompts: org.IncludePrompts, IncludeToolContent: org.IncludeToolContent, Signals: org.Signals,
		ExcludePaths: org.ExcludePaths, RequireClaim: !in.Primary || !org.Global()}
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
