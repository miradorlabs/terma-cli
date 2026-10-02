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
	Dir string
	// Addr is where to listen: what setup recorded when empty.
	Addr string
	// Idle is how long a quiet relay runs before it exits; zero is the service's relay, which never idles.
	Idle time.Duration
	// Environment is the backend environment the relay delivers to, recorded for doctor.
	Environment string
	// Engine is the engine's options; Run sets its Token.
	Engine relay.Options
	// Workers run beside the engine until it stops, and are waited for.
	Workers   []func(ctx context.Context)
	Listening func(addr net.Addr, hold time.Duration)
}

// Result is how a relay's run ended.
type Result struct {
	AlreadyRunning bool
	Service        bool
	// Replaced means a newer terma replaced this binary and the relay stepped aside.
	Replaced bool
	// SetupGone means the relay's token was removed: nothing to relay for.
	SetupGone bool
}

// Restart reports whether whatever runs the relay should start it again.
func (r Result) Restart() bool {
	return r.Replaced || r.Service && !r.SetupGone && !r.AlreadyRunning
}

// Run runs the relay until ctx ends, it is told to stop or it idles, delivering what it
// accepted and saving its counters before it returns.
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
	runPath := filepath.Join(c.Dir, RunFile)
	if data, err := json.Marshal(RunInfo{PID: os.Getpid(), Environment: c.Environment, Service: res.Service}); err == nil {
		_ = config.WriteFileAtomicNoSync(runPath, append(data, '\n'), 0o600)
	}
	defer func() { _ = os.Remove(runPath) }()

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

// RunInfo is what the running relay records about itself.
type RunInfo struct {
	PID int `json:"pid"`
	// Environment is the backend environment it delivers to.
	Environment string `json:"environment"`
	// Service is true for the service's relay, false for one a hook or a developer started.
	Service bool `json:"service"`
}

// RunningRelay is what the relay holding dir's lock recorded about itself; false when none
// runs, or it is a terma from before the record.
func RunningRelay(dir string) (RunInfo, bool) {
	if !Running(dir) {
		return RunInfo{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, RunFile))
	if err != nil {
		return RunInfo{}, false
	}
	var info RunInfo
	if json.Unmarshal(data, &info) != nil || info.PID <= 0 {
		return RunInfo{}, false
	}
	return info, true
}

// lockPoll is how often the service's relay retries a lock another relay holds. Nothing
// listens between that relay's exit and the retry, and an agent never resends what it
// exported into the gap, so the wait is short.
const lockPoll = 250 * time.Millisecond

// lock takes the single-instance lock, with wait waiting out a hook-started relay; a nil
// unlock means this relay must not run.
func lock(ctx context.Context, dir string, wait bool) (unlock func(), busy bool, err error) {
	path := filepath.Join(dir, LockFile)
	unlock, err = flock.TryLock(path)
	for wait && flock.IsBusy(err) {
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(lockPoll):
		}
		unlock, err = flock.TryLock(path)
	}
	if flock.IsBusy(err) {
		return nil, true, nil
	}
	return unlock, false, err
}

// listen writes a failure where status reads it, since a hook-started relay has nowhere to print.
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
		// A replaced binary steps aside only once quiet: agents do not retry a refused connection.
		if quiet && d >= time.Minute && self != "" && executableStamp() != self {
			return true, false, nil
		}
		if quiet && idle > 0 && d >= idle {
			return false, false, nil
		}
	}
}

// stopRequested takes Stop's request; one naming another pid is a dead relay's, and is cleared.
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

// executableStamp lets a relay tell its binary was replaced; empty when unknown.
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
