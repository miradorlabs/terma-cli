package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Config is what a relay runs with.
type Config struct {
	// Dir is the state directory (Dir).
	Dir string
	// Addr is where to listen: what setup recorded when empty.
	Addr string
	// Idle is how long the relay runs with no export and nothing held or queued before
	// it exits. Zero is the service's relay: it waits out a relay a hook started, and
	// never idles.
	Idle time.Duration
	// Engine is the engine's options; Run sets its Token.
	Engine relay.Options
	// Workers run beside the engine until it stops, and are waited for (the policy
	// refresher).
	Workers []func(ctx context.Context)
	// Listening is told the address once the relay listens.
	Listening func(addr net.Addr, hold time.Duration)
}

// Result is how a relay's run ended.
type Result struct {
	// AlreadyRunning: another relay holds the lock, and this one did not run.
	AlreadyRunning bool
	// Service: this was the service's relay (Config.Idle zero).
	Service bool
	// Replaced: a newer terma replaced this binary, and the relay stepped aside.
	Replaced bool
	// SetupGone: the relay's setup was undone (its token removed): nothing to relay for.
	SetupGone bool
}

// Restart reports whether whatever runs the relay should start it again: after it
// stepped aside for a replaced binary, and, for the service's relay, whenever it
// stopped for any reason but its setup being gone.
func (r Result) Restart() bool {
	return r.Replaced || r.Service && !r.SetupGone && !r.AlreadyRunning
}

// Run runs the relay until ctx ends, its setup is gone, a stop is requested, its binary
// is replaced or it has been idle for Config.Idle. What it accepted is delivered, its
// workers stopped and its counters saved before it returns.
func Run(ctx context.Context, c Config) (Result, error) {
	res := Result{Service: c.Idle <= 0}
	if res.Service {
		if _, err := Token(); err != nil {
			res.SetupGone = true
			return res, nil
		}
	}
	unlock, busy, err := lock(ctx, c.Dir, res.Service)
	if err != nil {
		return res, err
	}
	if unlock == nil {
		res.AlreadyRunning = busy
		return res, nil
	}
	defer unlock()
	token, err := Token()
	if err != nil {
		return res, err
	}
	addr := c.Addr
	if addr == "" {
		addr = Addr(c.Dir)
	}
	opts := c.Engine
	opts.Token = token
	r := relay.New(opts)
	ln, err := listen(c.Dir, addr)
	if err != nil {
		return res, err
	}
	if c.Listening != nil {
		c.Listening(ln.Addr(), opts.Hold)
	}
	claim.Prune(time.Now())
	pidPath := filepath.Join(c.Dir, PIDFile)
	_ = config.WriteFileAtomicNoSync(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	defer func() { _ = os.Remove(pidPath) }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A client that trickles its request cannot hold a connection open.
	srv := &http.Server{Handler: r.Handler(), ConnContext: r.ConnContext, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	engine := make(chan struct{})
	go func() { r.Run(ctx); close(engine) }()
	var workers sync.WaitGroup
	for _, w := range c.Workers {
		workers.Go(func() { w(ctx) })
	}

	var serveErr error
	res.Replaced, res.SetupGone, serveErr = watch(ctx, r, c.Dir, c.Idle, served)
	// Stop taking exports, let the ones in flight finish, then let the engine deliver
	// what it accepted before the workers and the process go.
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdown)
	cancel()
	<-engine
	workers.Wait()
	if data, err := json.MarshalIndent(r.Stats().Snapshot(), "", "  "); err == nil {
		_ = config.WriteFileAtomicNoSync(filepath.Join(c.Dir, StatsFile), append(data, '\n'), 0o600)
	}
	return res, serveErr
}

// lock takes the relay's single-instance lock. With wait (the service's relay) it
// waits out a relay a hook started. A nil unlock means this relay must not run: busy
// when another holds the lock, neither when ctx ended the wait.
func lock(ctx context.Context, dir string, wait bool) (unlock func(), busy bool, err error) {
	path := filepath.Join(dir, LockFile)
	unlock, err = flock.TryLock(path)
	for wait && flock.IsBusy(err) {
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(5 * time.Second):
		}
		unlock, err = flock.TryLock(path)
	}
	if flock.IsBusy(err) {
		return nil, true, nil
	}
	return unlock, false, err
}

// listen opens the relay's address. A hook started this relay with nowhere to print,
// so a failure is also written where status reads it (and Spawn backs off).
func listen(dir, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		err = fmt.Errorf("relay: listen on %s: %w", addr, err)
		_ = config.WriteFileAtomicNoSync(filepath.Join(dir, ErrorFile), []byte(time.Now().UTC().Format(time.RFC3339)+" "+err.Error()+"\n"), 0o600)
		return nil, err
	}
	_ = os.Remove(filepath.Join(dir, ErrorFile))
	return ln, nil
}

// watch checks the running relay every second until it should stop (ctx ended, its
// setup gone, a stop requested, its binary replaced, idle for idle, or its server
// failed) and says why.
func watch(ctx context.Context, r *relay.Relay, dir string, idle time.Duration, served <-chan error) (replaced, setupGone bool, err error) {
	self := executableStamp()
	lastPrune := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, false, nil
		case err := <-served:
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			return false, false, err
		case <-tick.C:
		}
		d, quiet := r.Idle()
		// Setup undone (terma uninstalled, the config directory removed): the agents no
		// longer point here, so there is nothing to relay for.
		if _, err := Token(); err != nil {
			return false, true, nil
		}
		if stopRequested(dir) {
			return false, false, nil
		}
		if time.Since(lastPrune) > time.Hour {
			claim.Prune(time.Now())
			lastPrune = time.Now()
		}
		// A newer terma replaced this binary (update, reinstall): step aside once quiet,
		// and the next hook or the service starts the new one. Not mid-export: agents do
		// not retry a refused connection.
		if quiet && d >= time.Minute && self != "" && executableStamp() != self {
			return true, false, nil
		}
		if quiet && idle > 0 && d >= idle {
			return false, false, nil
		}
	}
}

// stopRequested reports whether Stop asked this relay to stop through the stop file,
// and takes the request. One naming another pid is a dead relay's, and is cleared.
func stopRequested(dir string) bool {
	path := filepath.Join(dir, StopFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid == os.Getpid()
}

// executableStamp identifies the file this process was started from, its size and
// modification time, so a relay can tell it has been replaced. Empty when unknown.
func executableStamp() string {
	exe := procinfo.StableExecutable()
	if exe == "" {
		return ""
	}
	info, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
}
