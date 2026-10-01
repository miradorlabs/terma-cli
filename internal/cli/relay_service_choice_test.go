package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// install keeps only a service this terma would write; one an earlier terma wrote (a
// removed command, another binary or environment) is rewritten, never trusted for existing.
func TestInstallKeepsOnlyACurrentService(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state daemon.ServiceState
		flag  string
		keep  bool
	}{
		{"current", daemon.ServiceState{Installed: true, Current: true}, "", true},
		{"current, asked again", daemon.ServiceState{Installed: true, Current: true}, "on", false},
		{"stale: an earlier terma's", daemon.ServiceState{Installed: true}, "", false},
		{"not installed", daemon.ServiceState{}, "", false},
	} {
		if got := keepService(tc.state, tc.flag); got != tc.keep {
			t.Errorf("%s: keepService = %v, want %v", tc.name, got, tc.keep)
		}
	}
}
