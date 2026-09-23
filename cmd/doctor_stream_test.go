package cmd

import (
	"context"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// Each check is reported the moment it finishes, in order, so the command can print
// it then rather than after the slow round-trip at the end.
func TestDoctorReportsEachCheckAsItFinishes(t *testing.T) {
	userSandbox(t)
	var started, finished []string
	report := runDoctor(context.Background(), true, doctorProgress{
		start: func(name string) { started = append(started, name) },
		done: func(c doctor.Check) {
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
