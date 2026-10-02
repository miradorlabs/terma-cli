package daemon

import (
	"cmp"
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
	// Log, when set, is told of the relay's start and exit, with its counters.
	Log *Log
	// Successor is a relay a stopping one started to take its socket: it waits for that
	// one's lock as long as a drain takes.
	Successor bool
	// SpawnSuccessor starts such a relay, detached; nil starts none.
	SpawnSuccessor func() error
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
	unlock, busy, err := lock(ctx, c.Dir, res.Service, c.Successor)
	if err != nil {
		return res, err
	}
	if unlock == nil {
		res.AlreadyRunning = busy
		return res, nil
	}
	var unlockOnce sync.Once
	release := func() { unlockOnce.Do(unlock) }
	defer release()
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
		c.Log.Printf("relay pid %d: %v", os.Getpid(), err)
		return res, err
	}
	kind := "on demand"
	if res.Service {
		kind = "service"
	}
	c.Log.Printf("relay pid %d (%s, %s) listening on %s", os.Getpid(), kind, cmp.Or(c.Environment, "prod"), ln.Addr())
	if c.Listening != nil {
		c.Listening(ln.Addr(), opts.Hold)
	}
	claim.Prune(time.Now())
	pidPath := filepath.Join(c.Dir, PIDFile)
	_ = config.WriteFileAtomicNoSync(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	runPath := filepath.Join(c.Dir, RunFile)
	if data, err := json.Marshal(RunInfo{PID: os.Getpid(), Environment: c.Environment, Service: res.Service}); err == nil {
		_ = config.WriteFileAtomicNoSync(runPath, append(data, '\n'), 0o600)
	}
	// Gone before the lock is, so the next relay's records are never removed.
	forget := func() { _ = os.Remove(pidPath); _ = os.Remove(runPath) }
	defer forget()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A client that trickles its request cannot hold a connection open.
	var accepted fresh
	srv := &http.Server{Handler: r.Handler(), ConnContext: r.ConnContext, ConnState: accepted.track,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	g := &gate{Listener: ln}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(g) }()
	engine := make(chan struct{})
	go func() { r.Run(ctx); close(engine) }()
	var workers sync.WaitGroup
	for _, w := range c.Workers {
		workers.Go(func() { w(ctx) })
	}

	why, serveErr := watch(ctx, r, c.Dir, c.Idle, res.Service, served)
	res.Replaced = why == stopReplaced
	// A relay stopped once its token went (teardown) is done for good, however it was stopped.
	res.SetupGone = why == stopSetupGone || !setUp()
	var h *handoff
	if why.handsOff() && !res.SetupGone {
		h = startHandoff(c.Dir, addr, ln, c.SpawnSuccessor)
	}
	if why != stopServeFailed {
		g.stop()
		<-served
		accepted.await(newRequestWait)
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdown)
	cancel()
	<-engine
	workers.Wait()
	snap := r.Stats().Snapshot()
	if data, err := json.MarshalIndent(snap, "", "  "); err == nil {
		// The previous relay's exit counters stay beside this one's.
		statsPath := filepath.Join(c.Dir, StatsFile)
		_ = os.Rename(statsPath, filepath.Join(c.Dir, PrevStatsFile))
		_ = config.WriteFileAtomicNoSync(statsPath, append(data, '\n'), 0o600)
	}
	if line, err := json.Marshal(snap.Counters); err == nil {
		c.Log.Printf("relay pid %d stopped (%s): %s", os.Getpid(), why.describe(serveErr), line)
	}
	await := h.offer()
	forget()
	release()
	if await != nil {
		if await() {
			c.Log.Printf("relay pid %d handed its socket to the next relay", os.Getpid())
		} else {
			c.Log.Printf("relay pid %d: no relay took its socket", os.Getpid())
		}
	}
	return res, serveErr
}

// stopReason is why a relay stops.
type stopReason int

const (
	// stopAsked is a signal or a stop request.
	stopAsked stopReason = iota
	stopIdle
	stopReplaced
	stopSetupGone
	stopServeFailed
	// stopForService is an on-demand relay stepping aside for the service's.
	stopForService
)

// handsOff reports whether a relay stopping for r passes its socket on: one that idled,
// failed or lost its setup has nothing to keep listening for.
func (r stopReason) handsOff() bool {
	return r == stopAsked || r == stopReplaced || r == stopForService
}

// describe names how a run ended, for the relay log.
func (r stopReason) describe(err error) string {
	switch {
	case err != nil:
		return err.Error()
	case r == stopReplaced:
		return "its binary was replaced"
	case r == stopSetupGone:
		return "its setup was removed"
	case r == stopIdle:
		return "idle"
	case r == stopForService:
		return "the service's relay takes over"
	}
	return "asked to stop"
}

// setUp reports whether the relay's token is still there.
func setUp() bool {
	_, err := Token()
	return err == nil
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

// lockPoll is how often the service's relay retries a lock another relay holds. Where no
// socket is handed over (Windows), nothing listens between that relay's exit and the
// retry, so the wait is short.
const lockPoll = 250 * time.Millisecond

// lock takes the single-instance lock; a nil unlock means this relay must not run. The
// service's relay waits out another relay, saying so in WaitingFile, and a successor
// waits out the relay that started it.
func lock(ctx context.Context, dir string, service, successor bool) (unlock func(), busy bool, err error) {
	path := filepath.Join(dir, LockFile)
	waiting := filepath.Join(dir, WaitingFile)
	if service {
		defer func() { _ = os.Remove(waiting) }()
	}
	poll, deadline := lockPoll, time.Time{}
	if successor {
		poll, deadline = successorPoll, time.Now().Add(successorWait)
	}
	unlock, err = flock.TryLock(path)
	for (service || successor && time.Now().Before(deadline)) && flock.IsBusy(err) {
		if service {
			touchWaiting(dir)
		}
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(poll):
		}
		unlock, err = flock.TryLock(path)
	}
	if flock.IsBusy(err) {
		return nil, true, nil
	}
	return unlock, false, err
}

// listen takes the socket a stopping relay offers for addr, or listens afresh, writing a
// failure where status reads it, since a hook-started relay has nowhere to print.
func listen(dir, addr string) (net.Listener, error) {
	if ln := take(dir, addr); ln != nil {
		_ = os.Remove(filepath.Join(dir, ErrorFile))
		return ln, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		err = fmt.Errorf("relay: listen on %s: %w", addr, err)
		_ = config.WriteFileAtomicNoSync(filepath.Join(dir, ErrorFile), []byte(time.Now().UTC().Format(time.RFC3339)+" "+err.Error()+"\n"), 0o600)
		return nil, err
	}
	_ = os.Remove(filepath.Join(dir, ErrorFile))
	return ln, nil
}

func watch(ctx context.Context, r *relay.Relay, dir string, idle time.Duration, service bool, served <-chan error) (stopReason, error) {
	self := executableStamp()
	lastPrune := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return stopAsked, nil
		case err := <-served:
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			return stopServeFailed, err
		case <-tick.C:
		}
		d, quiet := r.Idle()
		if !setUp() {
			return stopSetupGone, nil
		}
		if stopRequested(dir) {
			return stopAsked, nil
		}
		if time.Since(lastPrune) > time.Hour {
			claim.Prune(time.Now())
			lastPrune = time.Now()
		}
		// Stepping aside only once quiet, so nothing waiting in memory for a claim is lost.
		if quiet && d >= time.Minute && self != "" && executableStamp() != self {
			return stopReplaced, nil
		}
		if quiet && !service && handoffSupported && serviceWaiting(dir) {
			return stopForService, nil
		}
		if quiet && idle > 0 && d >= idle {
			return stopIdle, nil
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
