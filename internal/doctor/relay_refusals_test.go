package doctor

import (
	"strings"
	"testing"
	"time"
)

// Refusals going on warn with the likely cause and the fix, pointing at the live agent
// environment check when it named the variables; refusals that stopped an hour ago, or none,
// pass; a relay that does not say when it last refused is taken at its counters.
func TestRefusalsCheckWarnsWhileTheRelayRefuses(t *testing.T) {
	t.Parallel()
	now := time.Now()
	since := now.Add(-24 * time.Hour)
	for _, tc := range []struct {
		name     string
		r        Refusals
		envNamed bool
		status   Status
		want     []string
	}{
		{"none", Refusals{Since: since}, false, Pass, []string{"has refused no export"}},
		{"a foreign token now", Refusals{Unauthorized: 161, Since: since, At: now.Add(-time.Minute)}, true, Warn,
			[]string{"161 export requests for a token not its own", "stale or foreign exporter header or token", "restart the agent sessions",
				"remove the exporter variable", "the live agent environment check above names"}},
		{"unreadable bodies", Refusals{Unreadable: 3, Since: since, At: now}, false, Warn, []string{"3 it could not read", "cannot read"}},
		{"stopped an hour ago", Refusals{Unauthorized: 9, Since: since, At: now.Add(-2 * time.Hour)}, false, Pass, []string{"none in the last hour"}},
		{"an earlier relay that does not say when", Refusals{Unauthorized: 9, Since: since}, false, Warn, []string{"9 export requests"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := RefusalsCheck(tc.r, now, tc.envNamed)
			if c.Status != tc.status {
				t.Fatalf("RefusalsCheck = %+v, want %v", c, tc.status)
			}
			for _, w := range tc.want {
				if !strings.Contains(c.Detail+" "+c.Fix, w) {
					t.Errorf("check %+v lacks %q", c, w)
				}
			}
			if !tc.envNamed && strings.Contains(c.Fix, "live agent environment") {
				t.Errorf("fix %q points at a check that named nothing", c.Fix)
			}
		})
	}
}
