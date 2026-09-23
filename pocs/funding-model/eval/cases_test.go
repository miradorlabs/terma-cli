package eval

import (
	"testing"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/sim"
)

// TestCases is the acceptance run: every preset over the same twenty seeds
// `make eval` uses, against its envelope.
func TestCases(t *testing.T) {
	for _, sc := range sim.Presets {
		c, ok := CaseFor(sc.Name)
		if !ok {
			t.Errorf("%s: preset without a case", sc.Name)
			continue
		}
		s := RunMany(sc, 20)
		for _, f := range Check(c, s) {
			t.Errorf("%s: %s", sc.Name, f)
		}
		t.Logf("%-28s naive %.1f%%  cold %.1f%% (worst %.1f%%)  acc %.2f  warm2nd %.1f%% (worst %.1f%%)  cold2nd %.1f%%",
			sc.Name, 100*s.Mean.NaiveDelta, 100*s.Mean.ColdDelta, 100*s.Worst.ColdDelta, s.Mean.ColdAccuracy,
			100*s.Mean.SecondWarmDelta, 100*s.Worst.SecondWarmDelta, 100*s.Mean.SecondColdDelta)
	}
}
