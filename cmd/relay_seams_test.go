package cmd

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay"
)

// TestMain keeps every test away from the machine's service manager: a test that runs
// `terma setup` must never install a launchd agent or a systemd unit. With the relay
// unsupported, setup configures agents to export straight to Terma; a test about the
// relay opts in with fakeRelay.
func TestMain(m *testing.M) {
	relaySupported = func() bool { return false }
	relayInstallService = func(context.Context, string) error { panic("a test reached the real relay service install") }
	relayRestartService = func(context.Context) error { panic("a test reached the real relay service restart") }
	relayService = func(context.Context) (relay.ServiceState, error) { return relay.ServiceState{}, nil }
	relayProbe = func(context.Context, relay.Config) (relay.Health, error) {
		return relay.Health{}, os.ErrDeadlineExceeded
	}
	os.Exit(m.Run())
}

// fakeRelay is a relay that is set up, running and answering, for the length of the test.
// It records the binary the service was installed with.
type fakeRelayState struct {
	config    relay.Config
	installed string
	restarts  int
	health    relay.Health
	running   bool
}

func fakeRelay(t *testing.T) *fakeRelayState {
	t.Helper()
	st := &fakeRelayState{
		config:  relay.Config{Port: 14399, Token: strings.Repeat("ab", 32)},
		health:  relay.Health{Version: "test"},
		running: true,
	}
	saved := []any{relaySupported, relayEnsure, relayLoad, relayInstallService, relayRestartService, relayService, relayProbe}
	t.Cleanup(func() {
		relaySupported = saved[0].(func() bool)
		relayEnsure = saved[1].(func() (relay.Config, error))
		relayLoad = saved[2].(func() (relay.Config, error))
		relayInstallService = saved[3].(func(context.Context, string) error)
		relayRestartService = saved[4].(func(context.Context) error)
		relayService = saved[5].(func(context.Context) (relay.ServiceState, error))
		relayProbe = saved[6].(func(context.Context, relay.Config) (relay.Health, error))
	})
	relaySupported = func() bool { return true }
	relayEnsure = func() (relay.Config, error) { return st.config, nil }
	relayLoad = func() (relay.Config, error) {
		if st.installed == "" {
			return relay.Config{}, relay.ErrNotConfigured
		}
		return st.config, nil
	}
	relayInstallService = func(_ context.Context, binary string) error {
		st.installed = binary
		return nil
	}
	relayRestartService = func(context.Context) error {
		st.restarts++
		return nil
	}
	relayService = func(context.Context) (relay.ServiceState, error) {
		return relay.ServiceState{Installed: st.installed != "", Running: st.installed != "" && st.running, Binary: st.installed}, nil
	}
	relayProbe = func(context.Context, relay.Config) (relay.Health, error) {
		if !st.running {
			return relay.Health{}, os.ErrDeadlineExceeded
		}
		return st.health, nil
	}
	return st
}
