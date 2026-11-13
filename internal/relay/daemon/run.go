package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
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
	// Migration, set, is this machine's to migrate to multi-org routing once the relay
	// listens (migrate).
	Migration *migrate.Machine
	// prune and pruneEvery replace prune and its period, for a test.
	prune      func(ctx context.Context, stateDir string, r *relay.Relay)
	pruneEvery time.Duration
}

// Result is how a relay's run ended.
type Result struct {
	AlreadyRunning bool
	Service        bool
	// Replaced means a newer terma asked the relay to make way, and it stepped aside.
	Replaced bool
	// Updated means the relay installed a newer terma in place of its own, or found another
	// install had put one there, and stopped to run it: Installed is its version.
	Updated   bool
	Installed string
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

// LastBeatDue reports whether the relay's caller sends the last heartbeat (ExitBeat), which
// can take seconds. A relay a hook or a developer started does, whatever stopped it. The
// service's does not: its manager starts the next relay only once this process exits, and
// nothing listens until then, whatever the reason; its counters since the last beat stay in
// stats.json.
func (r Result) LastBeatDue() bool { return !r.Service }

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
	lockCtx, poll := ctx, time.Duration(0)
	if res.Service {
		poll = lockPoll
	}
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
		poll = followPoll
		followWaiting()
	}
	marker := ""
	if res.Service {
		marker = ServiceWaitFile
	}
	unlock, busy, err := lock(lockCtx, dir, poll, marker)
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
	// A record there while this relay holds the lock is a dead relay's: Stop must not take
	// its pid for this relay's.
	_ = os.Remove(filepath.Join(dir, RunFile))
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
	workers.Go(func() { c.pruneLoop(engineCtx, r) })
	updated := make(chan Pass, 1)
	if c.Updater != nil {
		u := *c.Updater
		if u.Logf == nil {
			u.Logf = c.Log.Printf
		}
		workers.Go(func() { u.Run(engineCtx, updated) })
	}
	// Off the start path: the relay already listens under today's rules meanwhile.
	migrateCtx, stopMigrating := context.WithCancel(engineCtx)
	defer stopMigrating()
	var migrated chan error
	if c.Migration != nil {
		migrated = make(chan error, 1)
		workers.Go(func() { migrated <- c.migrate(migrateCtx) })
	}

	why, installed, serveErr := watch(ctx, r, c.StateDir, c.Idle, served, updated, migrated)
	stopMigrating()
	res.Replaced = why == stopReplaced
	res.Updated = why == stopUpdated || why == stopUpdatedByAnother
	if res.Updated {
		res.Installed = installed
	}
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

// pruneEvery is how often the relay prunes the claims and the run ledger.
const pruneEvery = time.Hour

// pruneLoop prunes once the relay listens, then every pruneEvery until ctx ends. It runs
// beside the watch loop: a prune reads both queues, and no stop, replacement or idle exit
// waits for it.
func (c Config) pruneLoop(ctx context.Context, r *relay.Relay) {
	run := c.prune
	if run == nil {
		run = prune
	}
	tick := time.NewTicker(cmp.Or(c.pruneEvery, pruneEvery))
	defer tick.Stop()
	for {
		run(ctx, c.StateDir, r)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// prune removes expired claims and prunes the run ledger (pruneRuns).
func prune(ctx context.Context, stateDir string, r *relay.Relay) {
	claim.Prune(stateDir, time.Now())
	pruneRuns(ctx, stateDir, r, time.Now())
}

// pruneRuns prunes the run ledger (runs.Prune), keeping every session a record still
// queued names: in the relay's outbox, and in the spool. While either cannot be read it
// prunes nothing, since a ledger pruned under a queued record would let it go.
func pruneRuns(ctx context.Context, stateDir string, r *relay.Relay, now time.Time) {
	if _, err := os.Stat(filepath.Join(stateDir, runs.Dir)); err != nil {
		return // no ledger: a machine not migrated keeps none
	}
	queued, err := r.QueuedSessions()
	if err != nil {
		return
	}
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		return
	}
	if sp.Sessions(ctx, func(id string) { queued[id] = true }) != nil {
		return
	}
	runs.Prune(stateDir, now, queued)
}
