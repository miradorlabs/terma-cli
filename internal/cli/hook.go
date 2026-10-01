package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// gitHookEvents are the only events not declared by an adapter; every name is committed
// wiring and must stay stable.
var gitHookEvents = map[string]agents.Handler{
	"prepare-commit-msg": hookrun.PrepareCommitMsg,
	"post-commit":        hookrun.PostCommit,
}

func (app *App) hookHandler(event string) (agents.Handler, bool) {
	if h, ok := gitHookEvents[event]; ok {
		return h, true
	}
	h, ok := app.agents.Handlers()[event]
	return h, ok
}

// flushesAfter has no throttle: one could strand the final turn until another hook fires.
// prepare-commit-msg never flushes, with its 50 ms budget.
func (app *App) flushesAfter(event string) bool {
	return event == "post-commit" || app.agents.FlushesAfter(event)
}

// HooksDisabled reports the kill switch `TERMA_HOOKS=0`, which makes every hook exit 0 at once.
func HooksDisabled() bool {
	return os.Getenv("TERMA_HOOKS") == "0"
}

func (app *App) newHookCommand() *cobra.Command {
	var user bool
	cmd := &cobra.Command{
		Use:    "hook <event> [args...]",
		Short:  "Internal: the runtime behind every installed hook shim",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		// Hooks exit 0 whatever happens, on top of the shims' `|| true`.
		RunE: func(cmd *cobra.Command, args []string) error {
			event := args[0]
			// A render hook still renders under TERMA_HOOKS=0, and ends the process with its
			// own exit status rather than returning through cobra.
			if render, ok := app.agents.Render(event); ok {
				os.Exit(app.runRender(cmd, render, args[1:], HooksDisabled()))
			}
			if HooksDisabled() {
				if off, ok := app.agents.WhenHooksOff(event); ok {
					_ = off(cmd.Context(), hookrun.Env{Now: time.Now(), Args: args[1:], Stderr: cmd.ErrOrStderr(), Debug: os.Getenv("TERMA_DEBUG") != ""})
				}
				return nil
			}
			handler, ok := app.hookHandler(event)
			if !ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "terma hook: unknown event %q (ignored)\n", event)
				return nil
			}
			policy := hookPolicy()
			if _, git := gitHookEvents[event]; !git && app.hookYields(user, policy, app.agents.ToolForEvent(event)) {
				return nil
			}
			cwd, err := os.Getwd()
			if err != nil {
				return nil
			}
			// Stdout is the hook's reply to the agent, which may feed it to the model. A
			// bounded copy of the payload lets a hook that spooled nothing still claim
			// its session (hookrun.ClaimFromPayload).
			payload := &boundedBuffer{max: 4 << 20}
			claimed := false
			env := hookrun.Env{
				Now:     time.Now(),
				Cwd:     cwd,
				Args:    args[1:],
				Stdin:   io.TeeReader(cmd.InOrStdin(), payload),
				OnClaim: func() { claimed = true },
				Stdout:  cmd.OutOrStdout(),
				Stderr:  cmd.ErrOrStderr(),
				Version: app.version,
				Debug:   os.Getenv("TERMA_DEBUG") != "",
				Spool:   openSpool(),
				Policy:  policy,
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			_ = handler(ctx, env)
			if !claimed {
				if s, ok := app.agents.PayloadSession(event, payload.Bytes()); ok {
					claimed = hookrun.ClaimFromPayload(ctx, env, s, app.agents.ToolForEvent(event))
				}
			}
			if claimed {
				daemon.Spawn()
				wireCloneOnFirstUse(ctx, cwd)
			}
			if app.flushesAfter(event) && env.Spool != nil {
				spawnFlush()
			}
			return nil
		},
	}
	// --user only before the event: what follows it is the event's own arguments.
	cmd.Flags().BoolVar(&user, "user", false, "internal: a machine-wide hook entry")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func (app *App) runRender(cmd *cobra.Command, render agents.RenderHandler, args []string, captureDisabled bool) int {
	cwd, _ := os.Getwd()
	env := hookrun.Env{
		Now:     time.Now(),
		Cwd:     cwd,
		Args:    args,
		Stdin:   cmd.InOrStdin(),
		Stdout:  cmd.OutOrStdout(),
		Stderr:  cmd.ErrOrStderr(),
		Version: app.version,
		Debug:   os.Getenv("TERMA_DEBUG") != "",
		Flush:   func() { spawnFlush() },
	}
	if !captureDisabled {
		env.Spool = openSpool()
	}
	return render(cmd.Context(), env)
}

// hookPolicy is one small local read; without a validated scope content capture stays off.
func hookPolicy() config.Policy {
	cfg, err := config.Load(config.Overrides{})
	if err != nil {
		return config.Policy{Mode: config.ModeRepo, Signals: []string{}}
	}
	if cfg.Policy.FetchedAt.IsZero() || cfg.Policy.TeamID == "" && os.Getenv("TERMA_POLICY_STUB") == "" {
		return config.Policy{Mode: config.ModeRepo, Signals: []string{}, OrganizationID: cfg.OrganizationID, AuthURL: cfg.AuthURL}
	}
	return cfg.Policy
}

// openSpool returns nil when the config dir cannot be used: a nil spool drops events,
// never failing a hook.
func openSpool() *spool.Spool {
	dir, err := config.Dir()
	if err != nil {
		return nil
	}
	s, err := spool.Open(filepath.Join(dir, "spool"))
	if err != nil {
		return nil
	}
	return s
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

// spawnFlush is silent on failure: the next flush picks up whatever this one leaves.
func spawnFlush() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	proc := exec.Command(exe, "spool", "flush", "--quiet")
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	procinfo.Detach(proc)
	if err := proc.Start(); err != nil {
		return
	}
	_ = proc.Process.Release()
}
