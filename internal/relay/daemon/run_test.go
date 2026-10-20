package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// setUpRelay is a config directory where `terma relay setup` wrote the token, with its
// relay directory dir.
func setUpRelay(t *testing.T) (stateDir, dir, token string) {
	t.Helper()
	stateDir = t.TempDir()
	dir, err := Dir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	token = claim.TokenPath(stateDir)
	if err := os.WriteFile(token, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return stateDir, dir, token
}

func runConfig(stateDir string, idle time.Duration, listening func(net.Addr, time.Duration)) Config {
	return Config{StateDir: stateDir, Addr: "127.0.0.1:0", Idle: idle, Listening: listening,
		Engine: relay.Options{Dir: filepath.Join(claim.Dir(stateDir), relay.OutboxDir)}}
}

func run(t *testing.T, c Config) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := Run(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("the relay ran until the test's deadline")
	}
	return res
}

// A quiet relay exits once idle, leaving its counters and no pid behind.
func TestARelayExitsWhenIdle(t *testing.T) {
	t.Parallel()
	stateDir, dir, _ := setUpRelay(t)
	var listened bool
	res := run(t, runConfig(stateDir, time.Millisecond, func(net.Addr, time.Duration) { listened = true }))
	if !listened || res.Replaced || res.Updated || res.SetupGone || res.AlreadyRunning || res.Service {
		t.Fatalf("Run = %+v, listened %v", res, listened)
	}
	if _, err := os.Stat(filepath.Join(dir, StatsFile)); err != nil {
		t.Errorf("no counters saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RunFile)); !os.IsNotExist(err) {
		t.Errorf("relay record left behind: %v", err)
	}
}

// Removing the token stops a running relay: there is nothing left to relay for.
func TestARelayStopsWhenItsSetupIsRemoved(t *testing.T) {
	t.Parallel()
	stateDir, _, token := setUpRelay(t)
	res := run(t, runConfig(stateDir, time.Hour, func(net.Addr, time.Duration) { _ = os.Remove(token) }))
	if !res.SetupGone || res.Restart() {
		t.Fatalf("Run = %+v", res)
	}
}

// One relay per state directory; a service relay without a token does not start at all.
func TestOnlyOneRelayRuns(t *testing.T) {
	t.Parallel()
	stateDir, dir, token := setUpRelay(t)
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if res := run(t, runConfig(stateDir, time.Hour, nil)); !res.AlreadyRunning {
		t.Fatalf("Run beside a running relay = %+v", res)
	}
	unlock()
	_ = os.Remove(token)
	c := runConfig(stateDir, 0, nil)
	c.Service = true
	if res := run(t, c); !res.SetupGone || !res.Service {
		t.Fatalf("service Run without a token = %+v", res)
	}
}

// A relay restarts after stepping aside for a newer terma, and a service relay unless
// its setup is gone; one that found another running does not.
func TestRestart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		res  Result
		want bool
	}{
		{Result{}, false},
		{Result{Replaced: true}, true},
		{Result{Service: true}, true},
		{Result{Service: true, SetupGone: true}, false},
		{Result{Service: true, Replaced: true, SetupGone: true}, true},
		{Result{Service: true, AlreadyRunning: true}, false},
		{Result{SetupGone: true}, false},
	} {
		if got := c.res.Restart(); got != c.want {
			t.Errorf("%+v.Restart() = %v, want %v", c.res, got, c.want)
		}
	}
}

// The service's relay waiting behind a hook-started one listens within a moment of that
// one's exit: nothing listens in between, and agents never resend what they exported into
// the gap. AwaitRelay sees a relay only while one holds the lock and listens.
func TestTheWaitingServiceRelayTakesOverAtOnce(t *testing.T) {
	t.Parallel()
	stateDir, dir, _ := setUpRelay(t)

	// The hook-started relay takes any free port, which the test records as setup would: a
	// port reserved and freed beforehand could go to another test's listener in between.
	hook := startRun(t, runConfig(stateDir, time.Hour, nil))
	hook.await(t, "the hook-started relay")
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(hook.addr.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !AwaitRelay(dir, time.Second) {
		t.Fatal("AwaitRelay misses a relay that holds the lock and listens")
	}

	c := runConfig(stateDir, 0, nil)
	c.Addr = "" // the address setup recorded
	c.Service = true
	service := startRun(t, c)
	for deadline := time.Now().Add(5 * time.Second); !serviceWaiting(dir); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the service relay never waited on the lock")
		}
	}
	// The port is free from the hook-started relay's exit until the service relay's next
	// try for the lock (lockPoll): a listener taking it then fails the test at once.
	hook.stop(t, "the hook-started relay")
	service.await(t, "the service relay")
	if gap := service.upAt.Sub(hook.doneAt); gap > time.Second {
		t.Fatalf("the service's relay listened %v after the hook-started one exited", gap)
	}
	if service.addr.String() != hook.addr.String() {
		t.Fatalf("the service's relay listened on %v, not the recorded %v", service.addr, hook.addr)
	}
	service.stop(t, "the service relay")
	if AwaitRelay(dir, 50*time.Millisecond) {
		t.Fatal("AwaitRelay reports a relay after both stopped")
	}
}
