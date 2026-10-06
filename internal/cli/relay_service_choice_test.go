package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// install keeps only a service this terma would write, with no relay for another
// environment holding its port; one an earlier terma wrote (a removed command, another
// binary or environment) is rewritten, never trusted for existing. A relay a hook started
// in front of a current service is not a reason to rewrite it: the service takes over.
func TestInstallKeepsOnlyACurrentService(t *testing.T) {
	current := daemon.ServiceState{Installed: true, Current: true}
	for _, tc := range []struct {
		name    string
		state   daemon.ServiceState
		foreign bool
		flag    string
		keep    bool
	}{
		{"current", current, false, "", true},
		{"current, asked again", current, false, "on", false},
		{"current, but a relay for another environment holds the port", current, true, "", false},
		{"stale: an earlier terma's", daemon.ServiceState{Installed: true}, false, "", false},
		{"not installed", daemon.ServiceState{}, false, "", false},
	} {
		if got := keepService(tc.state, tc.foreign, tc.flag); got != tc.keep {
			t.Errorf("%s: keepService = %v, want %v", tc.name, got, tc.keep)
		}
	}
}

// A relay is foreign when it delivers to another environment; one that recorded nothing
// (an earlier terma's) cannot be judged.
func TestForeignRelays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running daemon.RunInfo
		ok      bool
		env     string
		foreign bool
	}{
		{"none running", daemon.RunInfo{}, false, "dev", false},
		{"the service's, in this environment", daemon.RunInfo{Environment: "dev", Service: true}, true, "dev", false},
		{"another environment: delivers nothing here", daemon.RunInfo{Environment: "prod", Service: true}, true, "dev", true},
		{"hook-started, another environment", daemon.RunInfo{Environment: "prod"}, true, "dev", true},
		{"hook-started, this environment: handed to the service, not replaced", daemon.RunInfo{Environment: "dev"}, true, "dev", false},
		{"environment not recorded", daemon.RunInfo{Service: true}, true, "dev", false},
	} {
		if got := foreignRelay(tc.running, tc.ok, tc.env); got != tc.foreign {
			t.Errorf("%s: foreignRelay = %v, want %v", tc.name, got, tc.foreign)
		}
	}
}
