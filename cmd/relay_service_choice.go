package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The relay runs as a per-user service by default: `terma install` sets it up wherever
// a service manager can run it, because only an always-on relay receives what an agent
// exports before its first hook (Codex's conversation_starts, a Desktop thread's start)
// and what it exports while no hook has started one. macOS lists the service under
// Login Items, and says so once when it is added: the cost the developer pays for it.
// On Windows it is the per-user Run key and `terma relay supervise`.
// `terma install --relay-service off` (or `terma relay daemon remove`) opts out, and the
// choice is remembered (relay/no-service) until `--relay-service on` or `terma relay
// daemon install`; without the service, hooks start the relay on demand, as before.

// relayNoServiceFile, in the relay directory, records that the developer opted out.
const relayNoServiceFile = "no-service"

// relayServiceWanted says whether install should set the service up: --relay-service
// when given (recording the choice), else the choice recorded, else yes where a service
// can run. A test binary, and a test's `TERMA_RELAY_SERVICE=0` (the live suite, the
// install e2e matrix), never register one with the real service manager.
func relayServiceWanted(flag string) bool {
	dir, err := relayDir()
	if err != nil {
		return false
	}
	marker := filepath.Join(dir, relayNoServiceFile)
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
	return relayServiceSupported()
}

// ensureRelay leaves a relay running for the agents install just pointed at it: the
// service when wanted (installed, or reinstalled so it runs this build), else one started
// on demand. It reports what it did for install's output.
func ensureRelay(ctx context.Context, ui *installUI, flag string) {
	if !relayServiceWanted(flag) {
		if flag == "off" {
			if removed, _ := removeRelayService(ctx); removed {
				ui.ok("Relay", "service removed; hooks start the relay on demand")
			}
		}
		if _, ok := relayServiceInstalled(); !ok {
			spawnRelay()
		}
		return
	}
	if _, ok := relayServiceInstalled(); ok && flag != "on" {
		return
	}
	path, err := installRelayService(ctx)
	if err != nil {
		ui.warn("Relay", "could not run as a service ("+err.Error()+"); hooks start it on demand")
		spawnRelay()
		return
	}
	ui.ok("Relay", "runs in the background ("+path+"); `terma install --relay-service off` stops it")
}
