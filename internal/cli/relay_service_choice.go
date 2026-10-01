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
	if exe, err := os.Executable(); err != nil || strings.HasSuffix(filepath.Base(exe), ".test") {
		return false
	}
	return service.Supported()
}

// ensureRelay leaves a relay running, as a service when wanted, else started on demand,
// reporting only what the developer should know.
func ensureRelay(ctx context.Context, flag string, report func(warn bool, what string)) {
	if !relayServiceWanted(flag) {
		if flag == "off" {
			_, _ = daemon.RemoveService(ctx)
		}
		report(false, "starts when an agent needs it")
		daemon.Spawn()
		return
	}
	if _, ok := daemon.ServiceInstalled(); ok && flag != "on" {
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
	report(false, "running in the background")
	daemon.Spawn()
}
