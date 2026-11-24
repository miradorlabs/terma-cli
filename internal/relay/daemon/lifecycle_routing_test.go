package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

func TestEnsureRoutingRelayGatesPreRoutingRelays(t *testing.T) {
	origWait := preRoutingWait
	preRoutingWait = 120 * time.Millisecond
	t.Cleanup(func() { preRoutingWait = origWait })

	t.Run("no relay running succeeds", func(t *testing.T) {
		if err := EnsureRoutingRelay(t.TempDir(), "2.0.0"); err != nil {
			t.Fatalf("EnsureRoutingRelay(idle) = %v, want nil", err)
		}
	})

	t.Run("routing-capable relay succeeds without supersede", func(t *testing.T) {
		stateDir := t.TempDir()
		dir := claim.Dir(stateDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		writeRunInfo(t, dir, RunInfo{PID: os.Getpid(), Version: "2.0.0", Routing: true})
		if err := EnsureRoutingRelay(stateDir, "2.0.0"); err != nil {
			t.Fatalf("EnsureRoutingRelay(routing=true) = %v, want nil", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ReplaceFile)); !os.IsNotExist(err) {
			t.Fatalf("routing-capable relay should not have received ReplaceFile: err=%v", err)
		}
	})

	t.Run("pre-routing relay that refuses to exit blocks activation", func(t *testing.T) {
		stateDir := t.TempDir()
		dir := claim.Dir(stateDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		// PID == os.Getpid() so SIGTERM is not sent to the test process itself while the lock stays held.
		writeRunInfo(t, dir, RunInfo{PID: os.Getpid(), Version: "0.9.0", Routing: false})
		if err := EnsureRoutingRelay(stateDir, "2.0.0"); err == nil {
			t.Fatal("EnsureRoutingRelay(pre-routing stuck) = nil, want error")
		}
		if _, err := os.Stat(filepath.Join(dir, ReplaceFile)); err != nil {
			t.Fatalf("Supersede did not write ReplaceFile: %v", err)
		}
	})

	t.Run("pre-routing relay that releases lock on supersede succeeds", func(t *testing.T) {
		stateDir := t.TempDir()
		dir := claim.Dir(stateDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err != nil {
			t.Fatal(err)
		}
		released := make(chan struct{})
		defer func() { <-released }()
		writeRunInfo(t, dir, RunInfo{PID: os.Getpid(), Version: "0.9.0", Routing: false})
		go func() {
			defer close(released)
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(filepath.Join(dir, ReplaceFile)); err == nil {
					unlock()
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			unlock()
		}()
		if err := EnsureRoutingRelay(stateDir, "2.0.0"); err != nil {
			t.Fatalf("EnsureRoutingRelay(exits on supersede) = %v, want nil", err)
		}
	})
}

func writeRunInfo(t *testing.T, dir string, info RunInfo) {
	t.Helper()
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RunFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWithRoutingRelayGateHoldsRelayLockExclusively(t *testing.T) {
	origWait := preRoutingWait
	preRoutingWait = 120 * time.Millisecond
	t.Cleanup(func() { preRoutingWait = origWait })

	t.Run("holds relay lock across fn when idle", func(t *testing.T) {
		stateDir := t.TempDir()
		dir := claim.Dir(stateDir)
		called := false
		if err := WithRoutingRelayGate(stateDir, "2.0.0", func() error {
			called = true
			if unlock, err := flock.TryLock(filepath.Join(dir, LockFile)); err == nil {
				unlock()
				t.Fatal("relay lock was NOT held exclusively inside WithRoutingRelayGate")
			} else if !flock.IsBusy(err) {
				t.Fatalf("unexpected TryLock error: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatalf("WithRoutingRelayGate = %v, want nil", err)
		}
		if !called {
			t.Fatal("fn was not called")
		}
		// Lock must be released after WithRoutingRelayGate returns.
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err != nil {
			t.Fatalf("relay lock stayed held after return: %v", err)
		}
		unlock()
	})

	t.Run("refuses to run fn without lock when relay refuses to release", func(t *testing.T) {
		stateDir := t.TempDir()
		dir := claim.Dir(stateDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		writeRunInfo(t, dir, RunInfo{PID: os.Getpid(), Version: "2.0.0", Routing: true})
		called := false
		if err := WithRoutingRelayGate(stateDir, "2.0.0", func() error {
			called = true
			return nil
		}); err == nil {
			t.Fatal("WithRoutingRelayGate succeeded without acquiring relay lock")
		}
		if called {
			t.Fatal("fn ran while another process still held relay lock")
		}
	})
}
