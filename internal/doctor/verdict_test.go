package doctor

import (
	"errors"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// The verdict status and doctor share: the right host and another project is not
// connected, a machine-wide config exporting nothing leaves it to the repository, and a
// configuration that cannot emit reaches nothing whatever its route.
func TestJudgeHarness(t *testing.T) {
	const host, project = "https://ingest.example", "proj_1"
	sends := []harness.Signal{harness.SignalLogs}
	for _, c := range []struct {
		name         string
		facts        HarnessFacts
		route        Route
		bound, loose bool // Reaches in a bound repository, and outside one
	}{
		{"connected here", HarnessFacts{Status: harness.Status{Connected: true, Endpoint: host, ProjectID: project, Signals: sends}}, RouteGlobal, true, true},
		{"right host, another project", HarnessFacts{Status: harness.Status{Connected: true, Endpoint: host, ProjectID: "proj_2", Signals: sends}}, RouteOtherProject, false, false},
		{"another host", HarnessFacts{Status: harness.Status{Connected: true, Endpoint: "https://elsewhere", ProjectID: project, Signals: sends}}, RouteNone, false, false},
		{"exports repos, no repository policy", HarnessFacts{Status: harness.Status{Connected: true, Endpoint: host}}, RouteRepoDecides, false, true},
		{"exports repos, the repository asks", HarnessFacts{Status: harness.Status{Connected: true, Endpoint: host}, RepoAsks: true}, RouteRepoDecides, true, true},
		{"cannot emit", HarnessFacts{Status: harness.Status{Connected: true, Endpoint: host, Signals: sends}, EmissionProblem: "telemetry is disabled"}, RouteGlobal, false, false},
		{"unreadable", HarnessFacts{Status: harness.Status{Connected: true, Endpoint: host, Signals: sends}, Err: errors.New("parse")}, RouteNone, false, false},
	} {
		v := JudgeHarness(c.facts, host, project)
		if v.Route != c.route || v.Reaches(true) != c.bound || v.Reaches(false) != c.loose {
			t.Errorf("%s: route %v, reaches %v/%v; want %v, %v/%v", c.name, v.Route, v.Reaches(true), v.Reaches(false), c.route, c.bound, c.loose)
		}
	}
	if v := JudgeHarness(HarnessFacts{Status: harness.Status{Connected: true, Endpoint: host, ProjectID: "proj_2", Signals: sends}}, host, project); v.OtherProject != "proj_2" || v.SendsGlobally {
		t.Errorf("another project's verdict = %+v", v)
	}
}
