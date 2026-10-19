package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := func(idle time.Duration, listening func(net.Addr, time.Duration)) Config {
		c := runConfig(stateDir, idle, listening)
		c.Addr = ""
		return c
	}

	hookCtx, stopHook := context.WithCancel(t.Context())
	hookUp := make(chan struct{})
	hookDone := make(chan time.Time, 1)
	go func() {
		_, _ = Run(hookCtx, cfg(time.Hour, func(net.Addr, time.Duration) { close(hookUp) }))
		hookDone <- time.Now()
	}()
	<-hookUp
	if !AwaitRelay(dir, time.Second) {
		t.Fatal("AwaitRelay misses a relay that holds the lock and listens")
	}

	serviceCtx, stopService := context.WithCancel(t.Context())
	serviceUp := make(chan time.Time, 1)
	serviceDone := make(chan struct{})
	go func() {
		defer close(serviceDone)
		c := cfg(0, func(net.Addr, time.Duration) { serviceUp <- time.Now() })
		c.Service = true
		_, _ = Run(serviceCtx, c)
	}()
	time.Sleep(300 * time.Millisecond) // waiting on the lock
	stopHook()
	exited := <-hookDone
	select {
	case up := <-serviceUp:
		if gap := up.Sub(exited); gap > time.Second {
			t.Fatalf("the service's relay listened %v after the hook-started one exited", gap)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the service's relay never took over")
	}
	stopService()
	<-serviceDone
	if AwaitRelay(dir, 50*time.Millisecond) {
		t.Fatal("AwaitRelay reports a relay after both stopped")
	}
}

// A relay whose recorded address another process holds listens on a free port of the same
// host instead: it records that as its address, and the move, and says so (Config.Moved)
// for the agents to follow. What answered on the old address never counts as the relay.
// An address --addr names is not moved off.
func TestARelayMovesOffAnAddressAnotherProcessHolds(t *testing.T) {
	t.Parallel()
	stateDir, dir, _ := setUpRelay(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	from := held.Addr().String()
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(from+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Serving(dir) {
		t.Fatal("another process listening on the relay's address counts as the relay")
	}
	c := runConfig(stateDir, time.Hour, nil)
	c.Addr = ""
	moved := make(chan [2]string, 1)
	c.Moved = func(from, to string) { moved <- [2]string{from, to} }
	r := startRun(t, c)
	r.await(t, "the relay")
	var got [2]string
	select {
	case got = <-moved:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay never said it moved")
	}
	to := Addr(dir)
	if to == from || got != [2]string{from, to} {
		t.Fatalf("moved %v; recorded address %s, held %s", got, to, from)
	}
	if !Serving(dir) {
		t.Error("the relay does not serve the address it moved to")
	}
	if m, ok := RecentMove(dir, time.Now()); !ok || m.From != from || m.To != to {
		t.Errorf("RecentMove = %+v, %v", m, ok)
	}
	if _, ok := RecentMove(dir, time.Now().Add(MoveNotice)); ok {
		t.Error("a move is told of past MoveNotice")
	}
	if _, err := os.Stat(filepath.Join(dir, ErrorFile)); !os.IsNotExist(err) {
		t.Errorf("a relay that moved left its failure to listen behind: %v", err)
	}
	conn, err := net.DialTimeout("tcp", to, time.Second)
	if err != nil {
		t.Fatalf("nothing listens where the relay moved: %v", err)
	}
	_ = conn.Close()
	r.cancel()
	<-r.done

	c = runConfig(stateDir, time.Hour, nil)
	c.Addr = from
	if _, err := Run(t.Context(), c); err == nil {
		t.Fatal("a relay told to listen on a held address ran anyway")
	}
	if now := Addr(dir); now != to {
		t.Errorf("a relay told where to listen moved the recorded address to %s", now)
	}
}

// Serving is the relay's lock and its record of listening on the recorded address, by a
// live process: never a port that merely answers, nor a dead relay's record.
func TestServingIsTheRelaysLockAndRecord(t *testing.T) {
	_, dir, _ := setUpRelay(t)
	addr := freeAddr(t)
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Serving(dir) {
		t.Fatal("serving with no relay")
	}
	unlock := pretendRunning(t, dir, addr)
	if !Serving(dir) {
		t.Error("a relay holding the lock and recording its address is not serving")
	}
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(freeAddr(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Serving(dir) {
		t.Error("a relay listening elsewhere serves the recorded address")
	}
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dead := exec.Command(os.Args[0], "-test.run=^$")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RunFile), fmt.Appendf(nil, `{"pid":%d,"addr":%q}`, dead.Process.Pid, addr), 0o600); err != nil {
		t.Fatal(err)
	}
	if Serving(dir) {
		t.Error("a dead relay's record counts as serving")
	}
	unlock()
	if Serving(dir) {
		t.Error("serving once the lock is free")
	}
}
