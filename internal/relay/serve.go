package relay

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
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Options configure a running relay. Everything the relay needs from the rest of terma
// arrives here, so the package never reads a profile, a keystore or a binding itself.
type Options struct {
	// Config is the port and token (Load).
	Config Config
	// Dir is the state directory; Dir() when empty.
	Dir string
	// Version is this build, reported by /healthz and sent as the User-Agent.
	Version string
	// Binding names the project a directory is bound to.
	Binding BindingFunc
	// Destination says where a project's records go.
	Destination DestinationFunc
	// MachineProject names the project records go to when their session is in no bound
	// repository, or has no session.
	MachineProject MachineProjectFunc
	// Cwd finds a session's directory from the agent's own files; CodexCwd when nil.
	Cwd CwdFunc
	// Hold is how long a record whose session cannot be placed waits before it goes to
	// the machine project; 30 s when zero.
	Hold time.Duration
	// Logf receives the relay's diagnostics; discarded when nil.
	Logf func(format string, args ...any)
	// Client sends deliveries; a client with no overall timeout (each request has its
	// own) when nil.
	Client *http.Client
	// Listener, when set, is served instead of listening on Config.Port (tests).
	Listener net.Listener
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// DefaultHold is how long an unplaced record waits for its session to be placed.
const DefaultHold = 30 * time.Second

// ErrRunning is returned by Serve when another relay already serves this state directory.
var ErrRunning = errors.New("another relay is already running")

// Serve runs the relay until ctx ends: intake, router, forwarders and janitor. It picks
// up whatever an earlier run left in the inbox and outboxes.
func Serve(ctx context.Context, o Options) error {
	if o.Dir == "" {
		dir, err := Dir()
		if err != nil {
			return err
		}
		o.Dir = dir
	}
	if o.Binding == nil || o.Destination == nil || o.MachineProject == nil {
		return errors.New("relay: Binding, Destination and MachineProject are required")
	}
	if o.Cwd == nil {
		o.Cwd = CodexCwd
	}
	if o.Hold <= 0 {
		o.Hold = DefaultHold
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}

	for _, sub := range []string{inboxDir, outboxDir} {
		if err := os.MkdirAll(filepath.Join(o.Dir, sub), 0o700); err != nil {
			return err
		}
	}
	unlock, err := flock.TryLock(filepath.Join(o.Dir, "relay.lock"))
	if err != nil {
		if flock.IsBusy(err) {
			return ErrRunning
		}
		return err
	}
	defer unlock()

	ln := o.Listener
	if ln == nil {
		ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Config.Port)))
		if err != nil {
			return fmt.Errorf("listen on 127.0.0.1:%d: %w", o.Config.Port, err)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := newStats()
	fw := &forwarders{ctx: ctx, dir: o.Dir, dest: o.Destination, machine: o.MachineProject, client: o.Client,
		version: o.Version, logf: o.Logf, stats: st, m: map[string]chan struct{}{}}
	kick := make(chan struct{}, 1)
	wakeRouter := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	rt := &router{dir: o.Dir, res: newResolver(o.Dir, o.Binding, o.Cwd, o.Logf), traces: newTraceMap(8192),
		hold: o.Hold, now: o.Now, logf: o.Logf, stats: st, routed: fw.wake}
	in := &intake{dir: o.Dir, token: o.Config.Token, version: o.Version, now: o.Now, stats: st,
		logf: o.Logf, accepted: wakeRouter}

	// Deliver what an earlier run routed but did not send.
	if rs, err := routes(filepath.Join(o.Dir, outboxDir)); err == nil {
		for _, r := range rs {
			fw.wake(r)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// The router runs on a kick from intake, and on a tick for entries holding records
		// whose session is not placed yet.
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			rt.pass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-kick:
			case <-tick.C:
			}
		}
	}()
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			if n := sweep(o.Dir, o.Now()); n > 0 {
				st.addDropped(n)
				o.Logf("dropped %d queued bodies over the relay's bounds", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	srv := &http.Server{
		Handler:           in.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	o.Logf("relay %s serving on %s", o.Version, ln.Addr())

	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = srv.Shutdown(shutdown)
	cancel()
	wg.Wait()
	fw.wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// CodexCwd is the default CwdFunc: a Codex session's directory from its rollout header.
// Claude Code sessions are placed by their session-start hook alone; its transcript is
// conversation content the relay does not open.
func CodexCwd(ctx context.Context, session, source string) string {
	if source != "codex" {
		return ""
	}
	cwd, _ := harness.CodexRolloutCwd(ctx, session, "")
	return cwd
}

func probe(ctx context.Context, c Config) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint()+"/healthz", nil)
	if err != nil {
		return Health{}, err
	}
	req.Header.Set("Authorization", c.Authorization())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Health{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("relay answered HTTP %d", resp.StatusCode)
	}
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return Health{}, err
	}
	return h, nil
}
