package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

// The relay runs as a per-user service by default, since only an always-on relay receives
// what an agent exports before its first hook; daemon.NoServiceFile records an opt-out.

// relayServiceWanted reports whether install should set the service up: the flag
// (recorded), else the recorded choice, else where supported; never from a test.
func relayServiceWanted(flag string) bool {
	dir, err := daemon.Dir()
	if err != nil {
		return false
	}
	marker := filepath.Join(dir, daemon.NoServiceFile)
	switch flag {
	case "off":
		_ = os.MkdirAll(dir, 0o700)
		_ = config.WriteFileAtomic(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
		return false
	case "on":
		_ = os.Remove(marker)
	default:
		if _, err := os.Stat(marker); err == nil {
			return false
		}
	}
	if os.Getenv("TERMA_RELAY_SERVICE") == "0" {
		return false
	}
	if !realTerma() {
		return false
	}
	return service.Supported()
}

// ensureRelay leaves a relay running, as a service when wanted, else started on demand,
// reporting only what the developer should know. It is a developer's own command, so it
// records their environment as the relay's (daemon.RecordEnv).
func ensureRelay(ctx context.Context, flag string, report func(warn bool, what string)) {
	if dir, err := daemon.Dir(); err == nil {
		_ = daemon.RecordEnv(dir)
	}
	if !relayServiceWanted(flag) {
		if flag == "off" {
			_, _ = daemon.RemoveService(ctx)
		}
		report(false, "starts when an agent needs it")
		daemon.Spawn()
		return
	}
	// A definition that exists is not one that works: an earlier terma's may run a command
	// this one lacks, or another binary or environment. Only one this terma would write stays.
	state := daemon.CheckServiceHere()
	if keepService(state, flag) {
		// A service definition does not prove its relay is alive; the lock prevents duplicates.
		report(false, "running in the background")
		daemon.Spawn()
		return
	}
	if _, err := daemon.InstallService(ctx); err != nil {
		report(true, "could not run in the background ("+err.Error()+"); it starts when an agent needs it")
		daemon.Spawn()
		return
	}
	if state.Installed && !state.Current {
		report(false, "running in the background (its service was out of date and is rewritten for this terma)")
	} else {
		report(false, "running in the background")
	}
	daemon.Spawn()
}

// keepService reports whether install leaves the relay service as it is: only one this
// terma would write now, and only when --relay-service on did not ask for it again.
func keepService(state daemon.ServiceState, flag string) bool {
	return state.Installed && state.Current && flag != "on"
}

// realTerma is false for a test binary, which must not touch the machine's relay service.
func realTerma() bool {
	exe, err := os.Executable()
	return err == nil && !strings.HasSuffix(filepath.Base(exe), ".test")
}
