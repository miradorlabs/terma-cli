package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// install keeps only a service this terma would write, with no stray relay holding its
// port; one an earlier terma wrote (a removed command, another binary or environment) is
// rewritten, never trusted for existing.
func TestInstallKeepsOnlyACurrentService(t *testing.T) {
	current := daemon.ServiceState{Installed: true, Current: true}
	for _, tc := range []struct {
		name  string
		state daemon.ServiceState
		stray bool
		flag  string
		keep  bool
	}{
		{"current", current, false, "", true},
		{"current, asked again", current, false, "on", false},
		{"current, but a stray relay holds the port", current, true, "", false},
		{"stale: an earlier terma's", daemon.ServiceState{Installed: true}, false, "", false},
		{"not installed", daemon.ServiceState{}, false, "", false},
	} {
		if got := keepService(tc.state, tc.stray, tc.flag); got != tc.keep {
			t.Errorf("%s: keepService = %v, want %v", tc.name, got, tc.keep)
		}
	}
}

// A relay is stray when it delivers to another environment, or a hook started it while the
// service waits; one that recorded nothing (an earlier terma's) cannot be judged.
func TestStrayRelays(t *testing.T) {
	for _, tc := range []struct {
		name             string
		running          daemon.RunInfo
		ok               bool
		env              string
		serviceInstalled bool
		stray            bool
	}{
		{"none running", daemon.RunInfo{}, false, "dev", true, false},
		{"the service's, in this environment", daemon.RunInfo{Environment: "dev", Service: true}, true, "dev", true, false},
		{"another environment: delivers nothing here", daemon.RunInfo{Environment: "prod", Service: true}, true, "dev", true, true},
		{"another environment, no service", daemon.RunInfo{Environment: "prod"}, true, "dev", false, true},
		{"hook-started while the service waits", daemon.RunInfo{Environment: "dev"}, true, "dev", true, true},
		{"hook-started, no service: as designed", daemon.RunInfo{Environment: "dev"}, true, "dev", false, false},
		{"environment not recorded", daemon.RunInfo{Service: true}, true, "dev", true, false},
	} {
		if got := strayRelay(tc.running, tc.ok, tc.env, tc.serviceInstalled); got != tc.stray {
			t.Errorf("%s: strayRelay = %v, want %v", tc.name, got, tc.stray)
		}
	}
}
