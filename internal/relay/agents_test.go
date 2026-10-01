package relay

import (
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

var (
	testCorrelators = builtin.Agents().With[shape.Correlator]()
	testCapturers   = builtin.Agents().With[shape.Capturer]()
	testRules       = compose(testCorrelators, testCapturers)
)

// newRelay is New with every built-in agent's telemetry shape.
func newRelay(o Options) *Relay {
	o.Correlators, o.Capturers = testCorrelators, testCapturers
	return New(o)
}
