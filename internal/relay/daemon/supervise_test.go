package daemon

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

// supervisedChild re-runs this test binary into TestSupervisedChild as the supervised relay.
func supervisedChild(mode string) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^TestSupervisedChild$")
	c.Env = append(os.Environ(), "TERMA_SUPERVISED_CHILD="+mode)
	return c
}

func TestSupervisedChild(t *testing.T) {
	t.Parallel()
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

func testSupervisor() Supervisor {
	return Supervisor{Logf: func(string, ...any) {}, Stop: func() {}, MinPause: time.Millisecond, MaxPause: 4 * time.Millisecond,
		Healthy: time.Hour, Poll: 5 * time.Millisecond}
}

// The stop file stops the relay it names; a stale one naming another pid is cleared.
func TestStopFileStopsOnlyTheRelayItNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, StopFile)
	_ = os.WriteFile(path, []byte("1\n"), 0o600)
	if requested(dir, StopFile) {
		t.Fatal("a stop file naming another pid stopped this relay")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a stale stop file was left behind")
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	if !requested(dir, StopFile) {
		t.Fatal("a stop file naming this relay did not stop it")
	}
}

// A relay that exits asking to be restarted is started again while the service is installed.
func TestSuperviseRestartsTheRelayUntilRemoved(t *testing.T) {
	t.Parallel()
	var starts atomic.Int32
	sv := testSupervisor()
	sv.Start = func() *exec.Cmd { starts.Add(1); return supervisedChild("fail") }
	sv.Installed = func() bool { return starts.Load() < 3 }
	done := make(chan struct{})
	go func() { Supervise(context.Background(), sv); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor did not return once the service was removed")
	}
	if starts.Load() != 3 {
		t.Fatalf("started the relay %d times, want 3", starts.Load())
	}
}

// A relay that exits 0 is not started again, and the supervisor ends with it.
func TestSuperviseEndsWithARelayDoneForGood(t *testing.T) {
	t.Parallel()
	var starts atomic.Int32
	sv := testSupervisor()
	sv.Start = func() *exec.Cmd { starts.Add(1); return supervisedChild("done") }
	sv.Installed = func() bool { return true }
	done := make(chan struct{})
	go func() { Supervise(context.Background(), sv); close(done) }()
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
	t.Parallel()
	var removed, asked atomic.Bool
	var child *exec.Cmd
	started := make(chan struct{})
	sv := testSupervisor()
	sv.Start = func() *exec.Cmd { child = supervisedChild("wait"); close(started); return child }
	sv.Installed = func() bool { return !removed.Load() }
	sv.Stop = func() { asked.Store(true); _ = child.Process.Kill() }
	done := make(chan struct{})
	go func() { Supervise(context.Background(), sv); close(done) }()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor never started the relay")
	}
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
