package refdiff

import "testing"

// Why a check did not run is the run's message, not a change of the build.
func TestSkipReasonIsNoChange(t *testing.T) {
	cap := func(why string) *Snapshot {
		return &Snapshot{Harness: "t3", Version: why, Support: &Support{Status: "untested",
			Capabilities: []Capability{{ID: "relay.telemetry", Platform: "darwin/arm64", Result: "not run", Why: why}}}}
	}
	if got := compare(cap("t3 0.1.8 serves no orchestration API"), cap("")); len(got) != 0 {
		t.Errorf("changes = %+v", got)
	}
}
