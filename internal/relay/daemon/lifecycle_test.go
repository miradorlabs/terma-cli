package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Stop signals only a relay holding the lock: the pid a relay recorded before it crashed, or
// before a reboot, may belong to another process now.
func TestStopSignalsOnlyARelayHoldingTheLock(t *testing.T) {
	t.Parallel()
	_, dir, _ := setUpRelay(t)
	child := supervisedChild("wait")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	pid := child.Process.Pid
	if err := os.WriteFile(filepath.Join(dir, RunFile), fmt.Appendf(nil, `{"pid":%d}`, pid), 0o600); err != nil {
		t.Fatal(err)
	}

	Stop(dir)
	select {
	case <-exited:
		t.Fatal("Stop signalled the process a stale record names")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, StopFile)); !os.IsNotExist(err) {
		t.Fatalf("Stop asked the process a stale record names through the stop file: %v", err)
	}

	unlock, err := flock.TryLock(filepath.Join(dir, LockFile)) // the recorded relay holds it
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { Stop(dir); close(stopped) }()
	if runtime.GOOS == "windows" {
		// No SIGTERM there: the relay is asked through the stop file.
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if data, err := os.ReadFile(filepath.Join(dir, StopFile)); err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(pid) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Stop did not ask the relay holding the lock to stop")
			}
		}
	} else {
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Fatal("Stop did not signal the relay holding the lock")
		}
	}
	unlock()
	<-stopped
}

// A relay that takes the lock clears a dead relay's record before anything else, so the
// pid in it is never taken for its own.
func TestARelayClearsADeadRelaysRecord(t *testing.T) {
	t.Parallel()
	stateDir, dir, _ := setUpRelay(t)
	if err := os.WriteFile(filepath.Join(dir, RunFile), []byte(`{"pid":4242}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var left int
	run(t, runConfig(stateDir, time.Millisecond, func(net.Addr, time.Duration) { left = recordedPID(dir) }))
	if left != 0 {
		t.Fatalf("a starting relay kept the dead relay's record (pid %d)", left)
	}
}
