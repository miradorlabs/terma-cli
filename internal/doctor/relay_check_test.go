package doctor

import (
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// keyed is a keystore holding a key for every team.
func keyed(string, string) string { return "ter_srv_…1234" }

// healthy is a relay doctor passes: running as this terma's service, in this profile's environment.
func healthy() Relay {
	return Relay{Dir: "/tmp/relay", Addr: "127.0.0.1:43180", Running: true, Environment: "dev",
		ServiceInstalled: true, ServiceCurrent: true}
}

// The local relay is judged on what it delivers to and how it runs, not only on its port.
func TestRelayCheckJudgesTheRelayItself(t *testing.T) {
	reg := agents.New()
	for _, tc := range []struct {
		name   string
		relay  func(r *Relay)
		env    string
		status Status
		want   string
	}{
		{name: "healthy", relay: func(*Relay) {}, env: "dev", status: Pass, want: "only this repository's sessions are forwarded"},
		{name: "another environment: everything is dropped", relay: func(r *Relay) { r.Environment = "prod" }, env: "dev",
			status: Fail, want: "delivers to the prod environment, not this profile's dev"},
		{name: "environment unknown (an earlier terma's relay)", relay: func(r *Relay) { r.Environment = "" }, env: "dev", status: Pass},
		{name: "relay not running: nothing to compare", relay: func(r *Relay) { r.Running, r.Environment = false, "prod" }, env: "dev",
			status: Pass, want: "starts with the next hook"},
		{name: "service from an earlier terma", relay: func(r *Relay) { r.ServiceCurrent = false }, env: "dev",
			status: Warn, want: "written by an earlier terma"},
		{name: "hook-started relay holds the service off", relay: func(r *Relay) { r.HookStarted = true }, env: "dev",
			status: Warn, want: "the relay service waits behind it"},
		{name: "hook-started relay, no service: as designed", relay: func(r *Relay) { r.HookStarted, r.ServiceInstalled, r.ServiceCurrent = true, false, false },
			env: "dev", status: Pass},
		{name: "wrong environment outranks a stale service", relay: func(r *Relay) { r.Environment, r.ServiceCurrent = "prod", false }, env: "dev",
			status: Fail, want: "prod environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := healthy()
			tc.relay(&r)
			c := RelayCheck(reg, r, keyed, "proj_x", tc.env, nil)
			if c.Status != tc.status || !strings.Contains(c.Detail, tc.want) {
				t.Fatalf("RelayCheck = %+v, want %v containing %q", c, tc.status, tc.want)
			}
			if c.Status != Pass && c.Fix != "terma relay daemon install" {
				t.Fatalf("fix = %q, want the command that rewrites and restarts the service", c.Fix)
			}
		})
	}
}
