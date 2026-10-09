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
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Config is what a relay runs with.
type Config struct {
	// StateDir is terma's state directory; the relay keeps its files in claim.Dir of it.
	StateDir string
	// Addr is where to listen: what setup recorded when empty.
	Addr string
	// Idle is how long a quiet relay runs before it exits; zero never.
	Idle time.Duration
	// Service is the service manager's relay: it waits out another relay's lock, and asks to
	// be started again when it stops.
	Service bool
	// Follow is the relay Supersede starts as it asks the running one to make way: it waits
	// out that relay's lock, at most as long as the relay may take to go, and runs in its
	// place. One waits at a time (FollowLockFile).
	Follow bool
	// Launch is what started a relay that is not the service's, on the heartbeat and in
	// RunInfo: a hook, or a developer when empty. Service overrides it.
	Launch Launch
	// Environment is the backend environment the relay delivers to, recorded for doctor.
	Environment string
	// Version is this terma's, recorded so a newer one's hooks can ask the relay to make way.
	Version string
	// Engine is the engine's options; Run sets its Token.
	Engine relay.Options
	// Workers run beside the engine until it stops, and are waited for.
	Workers []func(ctx context.Context)
	// Updater, set, installs each new release in place of this terma, and the relay then
	// restarts on it.
	Updater   *Updater
	Listening func(addr net.Addr, hold time.Duration)
	// Log, when set, is told of the relay's start and exit, with its counters.
	Log *Log
}

// Result is how a relay's run ended.
type Result struct {
	AlreadyRunning bool
	Service        bool
	// Replaced means a newer terma asked the relay to make way, and it stepped aside.
	Replaced bool
	// Updated means the relay installed a newer terma in place of its own, and stopped to
	// run it.
	Updated bool
	// SetupGone means the relay's token was removed: nothing to relay for.
	SetupGone bool
	// exitBeat sends the relay's last heartbeat (ExitBeat); nil once setup is gone.
	exitBeat func()
}

// ExitBeat sends the stopped relay's last heartbeat, waiting at most exitBeatWait: why it
// stopped, and what it counted since the previous beat, what it dropped and left queued at
// exit included, which an idle exit or a teardown would otherwise lose. Run leaves it to its
// caller, to send once whatever starts the next relay is under way, since nothing listens
// meanwhile and an agent never sends a refused export again. A relay whose setup is gone
// sends none: teardown took away what it would report with.
func (r Result) ExitBeat() {
	if r.exitBeat != nil {
		r.exitBeat()
	}
}

// Restart reports whether whatever runs the relay should start it again.
func (r Result) Restart() bool {
	return r.Replaced || r.Updated || r.Service && !r.SetupGone && !r.AlreadyRunning
}

// Run runs the relay until ctx ends, it is told to stop or it idles, delivering what it
// accepted and saving its counters before it returns.
func Run(ctx context.Context, c Config) (Result, error) {
	res := Result{Service: c.Service}
	dir := claim.Dir(c.StateDir)
	if res.Service {
		if _, err := Token(c.StateDir); err != nil {
			res.SetupGone = true
			return res, nil
		}
	}
	lockCtx, wait := ctx, res.Service
	unfollow := func() {}
	if c.Follow && !res.Service {
		var err error
		if unfollow, err = flock.TryLock(filepath.Join(dir, FollowLockFile)); err != nil {
			// Another relay already waits to take over, or none can: not this one.
			res.AlreadyRunning = true
			return res, nil
		}
		var cancelWait context.CancelFunc
		lockCtx, cancelWait = context.WithTimeout(ctx, replacedMaxWait+followSlack)
		defer cancelWait()
		wait = true
	}
	unlock, busy, err := lock(lockCtx, dir, wait)
	// Whether it took over or gave up, the next relay to make way gets its own follower.
	unfollow()
	if err != nil {
		return res, err
	}
	if unlock == nil {
		res.AlreadyRunning = busy
		return res, nil
	}
	defer unlock()
	token, err := Token(c.StateDir)
	if err != nil {
		return res, err
	}
	addr := c.Addr
	if addr == "" {
		addr = Addr(dir)
	}
	ln, err := listen(dir, addr)
	if err != nil {
		c.Log.Printf("relay pid %d: %v", os.Getpid(), err)
		return res, err
	}
	opts := c.Engine
	opts.Token = token
	launch := cmp.Or(c.Launch, LaunchManual)
	if res.Service {
		launch = LaunchService
	}
	opts.Launch = string(launch)
	r := relay.New(opts)
	c.Log.Printf("relay pid %d (%s, %s) listening on %s", os.Getpid(), launch, cmp.Or(c.Environment, "prod"), ln.Addr())
	if c.Listening != nil {
		c.Listening(ln.Addr(), opts.Hold)
	}
	claim.Prune(c.StateDir, time.Now())
	runPath := filepath.Join(dir, RunFile)
	if data, err := json.Marshal(RunInfo{PID: os.Getpid(), Environment: c.Environment, Launch: launch, Service: res.Service, Version: c.Version}); err == nil {
		_ = config.WriteFileAtomicNoSync(runPath, append(data, '\n'), 0o600)
	}
	// Gone before the lock is, so the next relay's records are never removed.
	defer func() { _ = os.Remove(runPath) }()

	// The engine outlives a stop signal until the server has shut down, so its final sweep
	// sees, and its exit count includes, everything the last exports held.
	engineCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	// A client that trickles its request cannot hold a connection open.
	var accepted fresh
	// The connections whose handler may still hold a part. Only Serve's accept loop adds
	// one, and Serve has returned before Wait.
	var open sync.WaitGroup
	track := func(c net.Conn, s http.ConnState) {
		accepted.track(c, s)
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Done()
		}
	}
	srv := &http.Server{Handler: r.Handler(), ConnContext: r.ConnContext, ConnState: track,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	engine := make(chan struct{})
	go func() { r.Run(engineCtx); close(engine) }()
	var workers sync.WaitGroup
	for _, w := range c.Workers {
		workers.Go(func() { w(engineCtx) })
	}
	updated := make(chan string, 1)
	if c.Updater != nil {
		u := *c.Updater
		if u.Logf == nil {
			u.Logf = c.Log.Printf
		}
		workers.Go(func() { u.Run(engineCtx, updated) })
	}

	why, serveErr := watch(ctx, r, c.StateDir, c.Idle, served, updated)
	res.Replaced = why == stopReplaced
	res.Updated = why == stopUpdated
	// A relay stopped once its token went (teardown) is done for good, however it was stopped.
	res.SetupGone = why == stopSetupGone || !setUp(c.StateDir)
	if why != stopServeFailed {
		_ = ln.Close()
		<-served
		accepted.await(newRequestWait)
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if srv.Shutdown(shutdown) != nil {
		_ = srv.Close() // cuts off the requests the shutdown gave up waiting for
	}
	open.Wait()
	cancel()
	<-engine
	workers.Wait()
	snap := r.Stats().Snapshot()
	if data, err := json.MarshalIndent(snap, "", "  "); err == nil {
		// The previous relay's exit counters stay beside this one's.
		statsPath := filepath.Join(dir, StatsFile)
		_ = os.Rename(statsPath, filepath.Join(dir, PrevStatsFile))
		_ = config.WriteFileAtomicNoSync(statsPath, append(data, '\n'), 0o600)
	}
	if line, err := json.Marshal(snap.Counters); err == nil {
		c.Log.Printf("relay pid %d stopped (%s): %s", os.Getpid(), why.describe(serveErr), line)
	}
	if !res.SetupGone {
		exit := why.exit(serveErr)
		res.exitBeat = func() {
			ctx, cancel := context.WithTimeout(context.Background(), exitBeatWait)
			defer cancel()
			_ = r.StopHeartbeat(ctx, exit)
		}
	}
	return res, serveErr
}

// stopReason is why a relay stops.
type stopReason int

const (
	// stopAsked is a signal, or Stop's request where there are no signals (Windows).
	stopAsked stopReason = iota
	stopIdle
	stopReplaced
	stopUpdated
	stopSetupGone
	stopServeFailed
)

// describe names how a run ended, for the relay log.
func (r stopReason) describe(err error) string {
	switch {
	case err != nil:
		return err.Error()
	case r == stopReplaced:
		return "a newer terma asked it to make way"
	case r == stopUpdated:
		return "it installed a newer terma"
	case r == stopSetupGone:
		return "its setup was removed"
	case r == stopIdle:
		return "idle"
	}
	return "asked to stop"
}

// exitBeatWait bounds the last heartbeat, which a stopped relay's process waits for.
const exitBeatWait = 3 * time.Second

// exit is the last heartbeat's terma.relay.exit.reason for this run's end.
func (r stopReason) exit(err error) string {
	switch {
	case err != nil:
		return semconv.TermaRelayExitReasonServeFailed
	case r == stopReplaced:
		return semconv.TermaRelayExitReasonReplaced
	case r == stopUpdated:
		return semconv.TermaRelayExitReasonUpdated
	case r == stopSetupGone:
		return semconv.TermaRelayExitReasonSetupGone
	case r == stopIdle:
		return semconv.TermaRelayExitReasonIdle
	}
	return semconv.TermaRelayExitReasonAsked
}

// setUp reports whether the relay's token is still under stateDir.
func setUp(stateDir string) bool {
	_, err := Token(stateDir)
	return err == nil
}

// RunInfo is what the running relay records about itself.
type RunInfo struct {
	PID int `json:"pid"`
	// Environment is the backend environment it delivers to.
	Environment string `json:"environment"`
	// Launch is what started the relay: the service manager, a hook, or a developer.
	Launch Launch `json:"launch"`
	// Service is Launch == LaunchService, still written for a terma from before Launch,
	// and read from one: its record has no launch.
	Service bool `json:"service"`
	// Version is the terma it runs.
	Version string `json:"version,omitempty"`
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
	if info.Launch == "" && info.Service {
		info.Launch = LaunchService
	}
	return info, true
}

// lockPoll is how often the service's relay retries a lock another relay holds. Nothing
// listens between that relay's exit and the retry, so the wait is short.
const lockPoll = 250 * time.Millisecond

// lock takes the single-instance lock, with wait waiting out another relay; a nil unlock
// means this relay must not run.
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

// replacedMaxWait bounds how long a relay asked to make way waits for its hold to empty:
// the default hold, so an agent exporting without pause cannot keep the old terma running.
var replacedMaxWait = relay.DefaultHold

// followSlack is how long a following relay waits beyond replacedMaxWait, for the relay
// making way to deliver what it accepted and let go of its lock.
const followSlack = 30 * time.Second

// watch waits for a reason to stop. A relay that installed a newer terma stops once Quiesce
// finds its hold empty, no export in flight and none for updateQuiet (for none at all after
// updatePauseWait), however long that takes: the release is already in place for every
// hook, so waiting loses nothing, while what it holds in memory would be lost. Its queue is
// on disk, so a route still retrying never keeps it, and a newer hook asking it to make way
// is answered by that same wait.
func watch(ctx context.Context, r *relay.Relay, stateDir string, idle time.Duration, served <-chan error, updated <-chan string) (stopReason, error) {
	dir := claim.Dir(stateDir)
	var replacing time.Time // when a newer terma asked this relay to make way
	var upgraded time.Time  // when it installed one itself
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
		case <-updated:
			upgraded = time.Now()
		case <-tick.C:
		}
		if !setUp(stateDir) {
			return stopSetupGone, nil
		}
		if requested(dir, StopFile) {
			return stopAsked, nil
		}
		if replacing.IsZero() && requested(dir, ReplaceFile) {
			replacing = time.Now()
		}
		if !upgraded.IsZero() {
			quiet := updateQuiet
			if time.Since(upgraded) >= updatePauseWait {
				quiet = 0
			}
			if r.Quiesce(quiet) {
				return stopUpdated, nil
			}
		} else if !replacing.IsZero() && (!r.Holding() || time.Since(replacing) >= replacedMaxWait) {
			// Once the hold is empty, since what it keeps is lost at exit; the queue is on disk.
			return stopReplaced, nil
		}
		if time.Since(lastPrune) > time.Hour {
			claim.Prune(stateDir, time.Now())
			lastPrune = time.Now()
		}
		if d, quiet := r.Idle(); quiet && idle > 0 && d >= idle {
			return stopIdle, nil
		}
	}
}

// requested takes the request in dir's file name; one naming another pid is a dead
// relay's, and is cleared.
func requested(dir, name string) bool {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid == os.Getpid()
}
