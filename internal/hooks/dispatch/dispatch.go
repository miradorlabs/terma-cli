// Package dispatch runs `terma hook <event>`: the git hooks, then an agent's render hook,
// its hooks-off handler or its event handler, then the claim, the relay and the flush
// that follow. A hook never fails what called it; only a render hook has a status.
package dispatch

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// gitHookEvents are the events no agent declares; every name is committed wiring.
var gitHookEvents = map[string]agents.Event{
	"prepare-commit-msg": {Handler: hookrun.PrepareCommitMsg},
	"post-commit":        {Handler: hookrun.PostCommit, Flush: true},
}

// Request is one hook invocation.
type Request struct {
	Event string
	Args  []string
	// User marks a machine-wide hook entry.
	User           bool
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Version        string
	// HooksOff is the kill switch, TERMA_HOOKS=0: a render hook still renders, without
	// capture, and nothing else runs.
	Debug, HooksOff bool
	// Cwd is where the hook runs, empty when it cannot be told.
	Cwd string
}

// Deps are what a hook reaches beyond the hook runtime.
type Deps struct {
	Agents *agents.Registry
	// Policy is the collection policy hooks read locally.
	Policy func() config.Policy
	// Yields reports whether a repository's hook steps aside for a machine-wide one.
	Yields func(user bool, policy config.Policy, tool string) bool
	Spool  func() *spool.Spool
	// Claimed runs once a hook has claimed its session: the relay, the clone's wiring.
	Claimed func(ctx context.Context, cwd string)
	Flush   func()
}

// maxPayload bounds the copy of a payload kept to claim a session from.
const maxPayload = 4 << 20

// handlerTimeout bounds an event handler; a render hook keeps its own deadline.
const handlerTimeout = 5 * time.Second

// Run handles the request and returns the status the process exits with.
func Run(ctx context.Context, d Deps, r Request) int {
	if e, ok := gitHookEvents[r.Event]; ok {
		run(ctx, d, r, e.Handler, e.Flush, "")
		return 0
	}
	if render, ok := d.Agents.Render(r.Event); ok {
		env := hookrun.Env{Now: time.Now(), Cwd: r.Cwd, Args: r.Args, Stdin: r.Stdin, Stdout: r.Stdout, Stderr: r.Stderr,
			Version: r.Version, Debug: r.Debug, Flush: d.Flush}
		if !r.HooksOff {
			env.Spool = d.Spool()
		}
		return render(ctx, env)
	}
	if r.HooksOff {
		if off, ok := d.Agents.WhenHooksOff(r.Event); ok {
			_ = off(ctx, hookrun.Env{Now: time.Now(), Args: r.Args, Stderr: r.Stderr, Debug: r.Debug})
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
	policy := d.Policy()
	if tool != "" && d.Yields(r.User, policy, tool) {
		return
	}
	if r.Cwd == "" {
		return
	}
	// Stdout is the hook's reply to the agent, which may feed it to the model. A bounded
	// copy of the payload lets a hook that spooled nothing still claim its session.
	payload := &boundedBuffer{max: maxPayload}
	claimed := false
	env := hookrun.Env{
		Now:     time.Now(),
		Cwd:     r.Cwd,
		Args:    r.Args,
		Stdin:   io.TeeReader(r.Stdin, payload),
		OnClaim: func() { claimed = true },
		Stdout:  r.Stdout,
		Stderr:  r.Stderr,
		Version: r.Version,
		Debug:   r.Debug,
		Spool:   d.Spool(),
		Policy:  policy,
	}
	ctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()
	_ = handler(ctx, env)
	if !claimed && tool != "" {
		if s, ok := d.Agents.PayloadSession(r.Event, payload.Bytes()); ok {
			claimed = hookrun.ClaimFromPayload(ctx, env, s, tool)
		}
	}
	if claimed {
		d.Claimed(ctx, r.Cwd)
	}
	if flush && env.Spool != nil {
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
