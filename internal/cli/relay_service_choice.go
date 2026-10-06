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
func (app *App) relayServiceWanted(flag string) bool {
	dir, err := daemon.Dir(app.stateDir)
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

// ensureRelay leaves a relay running in env, as a service when wanted, else started on
// demand, reporting only what the developer should know. It is a developer's own command,
// so it records their environment as the relay's (daemon.RecordEnv), and it replaces a
// relay that cannot deliver for them: install is the one fix doctor names. It restarts
// nothing that already works, since an agent never resends what it exported while no relay
// listened.
func (app *App) ensureRelay(ctx context.Context, flag, env string, report func(warn bool, what string)) {
	dir, err := daemon.Dir(app.stateDir)
	if err == nil {
		_ = daemon.RecordEnv(dir)
	}
	running, isRunning := daemon.RunningRelay(dir)
	foreign := foreignRelay(running, isRunning, env)
	if !app.relayServiceWanted(flag) {
		if flag == "off" {
			_, _ = daemon.RemoveService(ctx, app.stateDir)
		}
		// A relay in another environment holds the lock, so the next one would wait behind it.
		if foreign && realTerma() {
			daemon.Stop(dir)
		}
		report(false, "starts when an agent needs it")
		daemon.Spawn(app.stateDir, app.version)
		return
	}
	// A definition that exists is not one that works: an earlier terma's may run a command
	// this one lacks, or another binary or environment. Only one this terma would write stays.
	state := daemon.CheckServiceHere(app.stateDir)
	if keepService(state, foreign, flag) {
		// The service's relay may have stopped, or wait behind a relay a hook started.
		if err := daemon.StartService(ctx, app.stateDir); err != nil {
			// A working relay is not stopped for a service that may never take its port.
			report(true, "could not start in the background ("+err.Error()+"); it starts when an agent needs it")
			if !isRunning {
				daemon.Spawn(app.stateDir, app.version)
			}
			return
		}
		report(false, "running in the background")
		if isRunning && !running.Service {
			daemon.Stop(dir)
		}
		app.awaitServiceRelay(dir)
		return
	}
	// Installing stops whatever relay holds the port, so the service takes over.
	if _, err := daemon.InstallService(ctx, app.stateDir); err != nil {
		report(true, "could not run in the background ("+err.Error()+"); it starts when an agent needs it")
		daemon.Spawn(app.stateDir, app.version)
		return
	}
	switch {
	case state.Installed && !state.Current:
		report(false, "running in the background (its service was out of date and is rewritten for this terma)")
	case foreign:
		report(false, "running in the background (replaced a relay that could not deliver for this profile)")
	default:
		report(false, "running in the background")
	}
	app.awaitServiceRelay(dir)
}

// awaitServiceRelay gives the service's relay time to take the lock and listen, and starts
// one on demand only if it does not: started at once, that one would take the port and
// leave the service's waiting behind it.
func (app *App) awaitServiceRelay(dir string) {
	if !daemon.AwaitRelay(dir, daemon.ServiceStartWait) {
		daemon.Spawn(app.stateDir, app.version)
	}
}

// foreignRelay reports whether the running relay delivers to another environment than
// env, so it cannot be left as it is. A relay from before relays recorded themselves (ok
// false) cannot be judged, and stays.
func foreignRelay(running daemon.RunInfo, ok bool, env string) bool {
	return ok && running.Environment != "" && env != "" && running.Environment != env
}

// keepService reports whether install leaves the relay service as it is: only one this
// terma would write now, with no relay for another environment holding its port, and only
// when --relay-service on did not ask for it again.
func keepService(state daemon.ServiceState, foreign bool, flag string) bool {
	return state.Installed && state.Current && !foreign && flag != "on"
}

// realTerma is false for a test binary, which must not touch the machine's relay service.
func realTerma() bool {
	exe, err := os.Executable()
	return err == nil && !strings.HasSuffix(filepath.Base(exe), ".test")
}
