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

// setUpRelay is a machine where `terma relay setup` wrote the token.
func setUpRelay(t *testing.T) (dir, token string) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if token, err = claim.TokenPath(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, token
}

func runConfig(dir string, idle time.Duration, listening func(net.Addr, time.Duration)) Config {
	return Config{Dir: dir, Addr: "127.0.0.1:0", Idle: idle, Listening: listening,
		Engine: relay.Options{Dir: filepath.Join(dir, relay.OutboxDir)}}
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
	dir, _ := setUpRelay(t)
	var listened bool
	res := run(t, runConfig(dir, time.Millisecond, func(net.Addr, time.Duration) { listened = true }))
	if !listened || res != (Result{}) {
		t.Fatalf("Run = %+v, listened %v", res, listened)
	}
	if _, err := os.Stat(filepath.Join(dir, StatsFile)); err != nil {
		t.Errorf("no counters saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); !os.IsNotExist(err) {
		t.Errorf("pid file left behind: %v", err)
	}
}

// Removing the token stops a running relay: there is nothing left to relay for.
func TestARelayStopsWhenItsSetupIsRemoved(t *testing.T) {
	dir, token := setUpRelay(t)
	res := run(t, runConfig(dir, time.Hour, func(net.Addr, time.Duration) { _ = os.Remove(token) }))
	if !res.SetupGone || res.Restart() {
		t.Fatalf("Run = %+v", res)
	}
}

// One relay per state directory; a service relay without a token does not start at all.
func TestOnlyOneRelayRuns(t *testing.T) {
	dir, token := setUpRelay(t)
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if res := run(t, runConfig(dir, time.Hour, nil)); !res.AlreadyRunning {
		t.Fatalf("Run beside a running relay = %+v", res)
	}
	unlock()
	_ = os.Remove(token)
	if res := run(t, runConfig(dir, 0, nil)); !res.SetupGone || !res.Service {
		t.Fatalf("service Run without a token = %+v", res)
	}
}

// A relay restarts after stepping aside for a replaced binary, and a service relay unless
// its setup is gone; one that found another running does not.
func TestRestart(t *testing.T) {
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
