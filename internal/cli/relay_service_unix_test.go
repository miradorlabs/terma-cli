//go:build unix

package cli

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

// A service relay (--service) that finds a hook's relay running waits and takes over.
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
	cmd := exec.CommandContext(ctx, bin, "relay", "run", "--service", "--idle", "0", "--quiet")
	cmd.Env = append(os.Environ(), "TERMA_CONFIG_DIR="+testApp.dir, "TERMA_STATE_DIR="+testApp.stateDir)
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

// A stopped service relay closes its port and exits ExitRestart to be restarted; with its
// setup gone it exits 0.
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
		cmd := exec.CommandContext(ctx, bin, "relay", "run", "--service", "--idle", "0", "--quiet")
		cmd.Env = append(os.Environ(), "TERMA_CONFIG_DIR="+testApp.dir, "TERMA_STATE_DIR="+testApp.stateDir)
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
	// Nothing listens until the service manager starts the relay again.
	if daemon.Running(dir) || daemon.Squatted(addr) {
		t.Fatal("a relay still listens after the service's relay was stopped")
	}
	if code := run(func(*exec.Cmd) { _ = os.Remove(filepath.Join(dir, "token")) }); code != 0 {
		t.Fatalf("setup gone: exit %d, want 0", code)
	}
}

// A service relay the service manager did not stop is stopped by `relay daemon remove`.
func TestDaemonRemoveStopsAServiceRelayLeftRunning(t *testing.T) {
	bin := termaBinary(t)
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "relay", "run", "--service", "--idle", "0", "--quiet")
	cmd.Env = append(os.Environ(), "TERMA_CONFIG_DIR="+testApp.dir, "TERMA_STATE_DIR="+testApp.stateDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	for deadline := time.Now().Add(10 * time.Second); !daemon.Running(dir) || !daemon.Squatted(addr); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the relay did not listen")
		}
	}
	if out, err := runTerma(t, "relay", "daemon", "remove"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the service relay kept running after its service was removed")
	}
	if daemon.Squatted(addr) {
		t.Fatal("something still listens on the relay's address")
	}
}
