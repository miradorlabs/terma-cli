package relay

import (
	"context"
	"testing"

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

// runRelay runs r until the test ends and waits for it to stop: a stopping relay writes
// what it still holds into its directory, which must not race the directory's removal.
func runRelay(tb testing.TB, r *Relay) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	tb.Cleanup(func() { cancel(); <-done })
}
