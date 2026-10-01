package cli

import (
	"context"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// Each check is reported in order the moment it finishes, not after the slow round-trip.
func TestDoctorReportsEachCheckAsItFinishes(t *testing.T) {
	userSandbox(t)
	var started, finished []string
	report := testApp.runDoctor(context.Background(), true, doctor.Progress{
		Start: func(name string) { started = append(started, name) },
		Done: func(c doctor.Check) {
			if len(finished) != len(started)-1 && len(finished) != len(started) {
				t.Errorf("check %q finished out of step with starts %v", c.Name, started)
			}
			finished = append(finished, c.Name)
		},
	})
	if len(finished) != len(report.Checks) {
		t.Fatalf("finished %d checks, report has %d", len(finished), len(report.Checks))
	}
	for i, c := range report.Checks {
		if finished[i] != c.Name {
			t.Fatalf("order differs at %d: streamed %q, report %q", i, finished[i], c.Name)
		}
	}
	if len(started) == 0 || started[0] != "terma on PATH" {
		t.Fatalf("the first check should be announced first: %v", started)
	}
}
