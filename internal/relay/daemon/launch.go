package daemon

import (
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Launch is what started the relay. Its values are the registry's terma.relay.launch.
type Launch string

const (
	// LaunchService is the service manager's relay (launchd, systemd, a Windows service).
	LaunchService Launch = semconv.TermaRelayLaunchService
	// LaunchHook is a relay terma started on demand: from an agent's hook (Spawn), or a
	// command such as setup or update.
	LaunchHook Launch = semconv.TermaRelayLaunchHook
	// LaunchManual is a relay a developer started from the command line.
	LaunchManual Launch = semconv.TermaRelayLaunchManual
)

// String is the value on the wire and in the relay log.
func (l Launch) String() string { return string(l) }

// Set reads a --launch flag value: hook or manual, since --service says the service's.
func (l *Launch) Set(v string) error {
	switch Launch(v) {
	case LaunchHook, LaunchManual:
		*l = Launch(v)
		return nil
	}
	return fmt.Errorf("want hook or manual, not %q", v)
}

// Type is the flag's, for help text.
func (l Launch) Type() string { return "launch" }
