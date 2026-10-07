// Package dispatch runs `terma hook <event>`: the git hooks, then an agent's render hook,
// its hooks-off handler or its event handler, then the claim, the relay and the flush
// that follow. A hook never fails what called it; only a render hook has a status.
package dispatch

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// gitHookEvents are the events no agent declares; every name is committed wiring.
var gitHookEvents = map[string]agents.Event{
	"prepare-commit-msg": {Handler: hookrun.PrepareCommitMsg},
	"post-commit":        {Handler: hookrun.PostCommit, Flush: true},
	"pre-push":           {Handler: hookrun.PrePush, Flush: true},
}

// Request is one hook invocation.
type Request struct {
	Event          string
	Args           []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Version        string
	// HooksOff is the kill switch, TERMA_HOOKS=0: a render hook still renders, without
	// capture, and nothing else runs. Run also sets it once terma is torn down.
	Debug, HooksOff bool
	// Cwd is where the hook runs, empty when it cannot be told.
	Cwd string
}

// Deps are what a hook reaches beyond the hook runtime.
type Deps struct {
	// ConfigDir is terma's config directory and StateDir its state directory, where hooks
	// keep their state.
	ConfigDir, StateDir string
	Agents              *agents.Registry
	// Profile is the developer's setup as hooks read it locally.
	Profile func() Profile
	Spool   func() *spool.Spool
	// Claimed runs once a hook has claimed its session, with the checkout it claimed and
	// the policy in force: the relay, and that repository's commit hooks.
	Claimed func(cwd string, policy config.Policy)
	Flush   func()
}

// Profile is what a hook reads of the developer's setup.
type Profile struct {
	Team   string
	Agents []string
	// Policy is the collection policy, NoPolicy until one is validated.
	Policy config.Policy
}

// maxPayload bounds the copy of a payload kept to claim a session from.
const maxPayload = 4 << 20

// handlerTimeout bounds an event handler; a render hook keeps its own deadline.
const handlerTimeout = 5 * time.Second

// Run handles the request and returns the status the process exits with.
func Run(ctx context.Context, d Deps, r Request) int {
	// Teardown retires the relay's token but leaves the commit hooks in repositories, and an
	// agent already running keeps the hooks it loaded: without the token, hooks are off.
	r.HooksOff = r.HooksOff || !claim.Enabled(d.StateDir)
	if e, ok := gitHookEvents[r.Event]; ok {
		run(ctx, d, r, e.Handler, e.Flush, "")
		return 0
	}
	if render, ok := d.Agents.Render(r.Event); ok {
		p := d.Profile()
		env := hookrun.Env{Now: time.Now(), Cwd: r.Cwd, Args: r.Args, Stdin: r.Stdin, Stdout: r.Stdout, Stderr: r.Stderr,
			ConfigDir: d.ConfigDir, StateDir: d.StateDir, Version: r.Version, Debug: r.Debug, Flush: d.Flush, Policy: p.Policy, Team: p.Team}
		if !r.HooksOff {
			env.Spool = d.Spool()
		}
		return render(ctx, env)
	}
	if r.HooksOff {
		if off, ok := d.Agents.WhenHooksOff(r.Event); ok {
			_ = off(ctx, hookrun.Env{Now: time.Now(), Args: r.Args, Stderr: r.Stderr, ConfigDir: d.ConfigDir, StateDir: d.StateDir, Debug: r.Debug})
		}
		return 0
	}
	e, ok := d.Agents.Event(r.Event)
	if !ok {
		fmt.Fprintf(r.Stderr, "terma hook: unknown event %q (ignored)\n", r.Event)
		return 0
	}
	run(ctx, d, r, e.Handler, e.Flush, agents.Tool(e.Agent))
	return 0
}

func run(ctx context.Context, d Deps, r Request, handler agents.Handler, flush bool, tool string) {
	if r.HooksOff {
		return
	}
	p := d.Profile()
	if r.Cwd == "" {
		return
	}
	// Stdout is the hook's reply to the agent, which may feed it to the model. A bounded
	// copy of the payload lets a hook that spooled nothing still claim its session.
	payload := &boundedBuffer{max: maxPayload}
	// The checkout to act on is the one the claim names, which the hook's own working
	// directory may not be: cmd starts a hook in a UNC checkout from the Windows directory.
	claimed, claimedIn, flushed := false, r.Cwd, false
	env := hookrun.Env{
		Now:       time.Now(),
		Cwd:       r.Cwd,
		Args:      r.Args,
		Stdin:     io.TeeReader(r.Stdin, payload),
		OnClaim:   func(root string) { claimed, claimedIn = true, root },
		Flush:     func() { flushed = true; d.Flush() },
		Stdout:    r.Stdout,
		Stderr:    r.Stderr,
		ConfigDir: d.ConfigDir,
		StateDir:  d.StateDir,
		Version:   r.Version,
		Debug:     r.Debug,
		Spool:     d.Spool(),
		Policy:    p.Policy,
		Team:      p.Team,
		Agents:    p.Agents,
	}
	ctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()
	_ = handler(ctx, env)
	if !claimed && tool != "" {
		if s, ok := d.Agents.PayloadSession(r.Event, payload.Bytes()); ok {
			claimed, claimedIn = hookrun.ClaimFromPayload(ctx, env, s, tool), cmp.Or(s.Cwd, r.Cwd)
		}
	}
	if claimed {
		d.Claimed(claimedIn, p.Policy)
	}
	if flush && env.Spool != nil && !flushed {
		d.Flush()
	}
}

// boundedBuffer keeps the first max bytes, so an oversized payload cannot inflate it.
type boundedBuffer struct {
	buf []byte
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

// Bytes returns what was kept.
func (b *boundedBuffer) Bytes() []byte { return b.buf }
