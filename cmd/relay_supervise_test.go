package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// supervisedChild is the relay a supervisor test runs: this test binary, re-run into
// TestSupervisedChild, which exits at once (0 for "done", 75 asking to be restarted
// for "fail") or waits to be stopped.
func supervisedChild(mode string) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^TestSupervisedChild$")
	c.Env = append(os.Environ(), "TERMA_SUPERVISED_CHILD="+mode)
	return c
}

func TestSupervisedChild(t *testing.T) {
	switch os.Getenv("TERMA_SUPERVISED_CHILD") {
	case "":
		t.Skip("run only as a supervisor test's child")
	case "wait":
		time.Sleep(time.Minute)
	case "fail":
		os.Exit(75)
	}
	os.Exit(0)
}

func testSupervisor() supervisor {
	return supervisor{logf: func(string, ...any) {}, stop: func() {}, minPause: time.Millisecond, maxPause: 4 * time.Millisecond,
		healthy: time.Hour, poll: 5 * time.Millisecond}
}

// The stop file (how stopRelay reaches a relay on Windows) stops the relay it names, and
// a stale one naming another pid is cleared without stopping anyone.
func TestStopFileStopsOnlyTheRelayItNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, relayStopFile)
	_ = os.WriteFile(path, []byte("1\n"), 0o600)
	if stopRequested(dir) {
		t.Fatal("a stop file naming another pid stopped this relay")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a stale stop file was left behind")
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	if !stopRequested(dir) {
		t.Fatal("a stop file naming this relay did not stop it")
	}
}

// A relay that exits asking to be restarted is started again, for as long as the
// service is installed.
func TestSuperviseRestartsTheRelayUntilRemoved(t *testing.T) {
	var starts atomic.Int32
	sv := testSupervisor()
	sv.start = func() *exec.Cmd { starts.Add(1); return supervisedChild("fail") }
	sv.installed = func() bool { return starts.Load() < 3 }
	done := make(chan struct{})
	go func() { superviseRelay(context.Background(), sv); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor did not return once the service was removed")
	}
	if starts.Load() != 3 {
		t.Fatalf("started the relay %d times, want 3", starts.Load())
	}
}

// A relay that exits 0 is done for good (its token is gone): it is not started again,
// and the supervisor ends with it.
func TestSuperviseEndsWithARelayDoneForGood(t *testing.T) {
	var starts atomic.Int32
	sv := testSupervisor()
	sv.start = func() *exec.Cmd { starts.Add(1); return supervisedChild("done") }
	sv.installed = func() bool { return true }
	done := make(chan struct{})
	go func() { superviseRelay(context.Background(), sv); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor kept running after the relay exited 0")
	}
	if starts.Load() != 1 {
		t.Fatalf("started the relay %d times, want 1", starts.Load())
	}
}

// Removing the service stops the running relay — asked first — and the supervisor.
func TestSuperviseStopsTheRelayWhenRemoved(t *testing.T) {
	var removed, asked atomic.Bool
	var child *exec.Cmd
	sv := testSupervisor()
	sv.start = func() *exec.Cmd { child = supervisedChild("wait"); return child }
	sv.installed = func() bool { return !removed.Load() }
	sv.stop = func() { asked.Store(true); _ = child.Process.Kill() }
	done := make(chan struct{})
	go func() { superviseRelay(context.Background(), sv); close(done) }()
	time.Sleep(100 * time.Millisecond)
	removed.Store(true)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor kept running after the service was removed")
	}
	if !asked.Load() {
		t.Fatal("the relay was not asked to stop")
	}
}
