//go:build unix

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// The service's relay (--idle 0) that finds a hook's relay running waits for it to
// exit and takes over, rather than exiting and leaving the machine without a service.
func TestServiceRelayTakesOverFromAHooksRelay(t *testing.T) {
	bin := termaBinary(t) // built before the sandbox moves HOME, and Go's caches with it
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "relay", "run", "--idle", "0", "--quiet")
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Signal(os.Interrupt); _ = cmd.Wait() }()
	time.Sleep(time.Second)
	if squatted(addr) {
		t.Fatal("the service relay started beside a running one")
	}
	unlock()
	for deadline := time.Now().Add(10 * time.Second); !squatted(addr); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the service relay did not take over")
		}
	}
}
