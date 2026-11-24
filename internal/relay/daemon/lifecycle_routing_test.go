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
	if GateStopWait < 11*time.Second {
		t.Fatalf("GateStopWait = %v, want >= 11s to cover normal relay shutdown", GateStopWait)
	}
	origWait := GateStopWait
	GateStopWait = 120 * time.Millisecond
	t.Cleanup(func() { GateStopWait = origWait })

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
	origWait := GateStopWait
	GateStopWait = 120 * time.Millisecond
	t.Cleanup(func() { GateStopWait = origWait })

	t.Run("holds relay lock across fn when idle without spawning", func(t *testing.T) {
		restarted := 0
		origRestart := restartRelay
		restartRelay = func(string, string) { restarted++ }
		t.Cleanup(func() { restartRelay = origRestart })

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
		if restarted != 0 {
			t.Fatalf("restartRelay called %d times when no relay was running; want 0", restarted)
		}
		// Lock must be released after WithRoutingRelayGate returns.
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err != nil {
			t.Fatalf("relay lock stayed held after return: %v", err)
		}
		unlock()
	})

	t.Run("restarts stopped relay after releasing lock even when fn fails", func(t *testing.T) {
		stateDir := t.TempDir()
		dir := claim.Dir(stateDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err != nil {
			t.Fatal(err)
		}
		writeRunInfo(t, dir, RunInfo{PID: os.Getpid(), Version: "2.0.0", Routing: true})
		go func() {
			time.Sleep(30 * time.Millisecond)
			unlock()
		}()

		restarted := 0
		lockFreeOnRestart := false
		origRestart := restartRelay
		restartRelay = func(string, string) {
			restarted++
			if u, err := flock.TryLock(filepath.Join(dir, LockFile)); err == nil {
				lockFreeOnRestart = true
				u()
			}
		}
		t.Cleanup(func() { restartRelay = origRestart })

		wantErr := os.ErrPermission
		if err := WithRoutingRelayGate(stateDir, "2.0.0", func() error {
			return wantErr
		}); err != wantErr {
			t.Fatalf("WithRoutingRelayGate = %v, want %v", err, wantErr)
		}
		if restarted != 1 || !lockFreeOnRestart {
			t.Fatalf("restartRelay called=%d lockFreeOnRestart=%v, want 1 and true", restarted, lockFreeOnRestart)
		}
	})

	t.Run("refuses to run fn without lock when routing relay refuses to release", func(t *testing.T) {
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
		err = WithRoutingRelayGate(stateDir, "2.0.0", func() error {
			called = true
			return nil
		})
		if err == nil {
			t.Fatal("WithRoutingRelayGate succeeded without acquiring relay lock")
		}
		if called {
			t.Fatal("fn ran while another process still held relay lock")
		}
		if err.Error() != "the running relay did not release its lock in time; retry once it stops" {
			t.Fatalf("unexpected error for routing-capable relay lock timeout: %v", err)
		}
	})
}
