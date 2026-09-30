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
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
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
	unlock, err := flock.TryLock(filepath.Join(dir, daemon.LockFile))
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
	if daemon.Squatted(addr) {
		t.Fatal("the service relay started beside a running one")
	}
	unlock()
	for deadline := time.Now().Add(10 * time.Second); !daemon.Squatted(addr); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the service relay did not take over")
		}
	}
}

// The service's relay tells its manager whether to start it again: stopped (by `terma
// relay setup`, to reread its setup) it exits ExitRestart; with its setup gone (terma
// uninstalled) it exits 0 and stays stopped.
func TestServiceRelayExitCodes(t *testing.T) {
	bin := termaBinary(t)
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	run := func(stop func(*exec.Cmd)) int {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "relay", "run", "--idle", "0", "--quiet")
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); !daemon.Squatted(addr); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the relay did not listen")
			}
		}
		stop(cmd)
		_ = cmd.Wait()
		return cmd.ProcessState.ExitCode()
	}
	if code := run(func(*exec.Cmd) { daemon.Stop(dir) }); code != ExitRestart {
		t.Fatalf("stopped: exit %d, want %d", code, ExitRestart)
	}
	if code := run(func(*exec.Cmd) { _ = os.Remove(filepath.Join(dir, "token")) }); code != 0 {
		t.Fatalf("setup gone: exit %d, want 0", code)
	}
}
