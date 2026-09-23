package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// gitHookEvents are the two git hooks, run by the committed shims or the hook manager
// line. Every other event belongs to an agent adapter and is dispatched from the
// adapter registry: the names are part of the committed wiring and must stay stable
// across versions, which is why each adapter declares its own.
var gitHookEvents = map[string]adapter.Handler{
	"prepare-commit-msg": hookrun.PrepareCommitMsg,
	"post-commit":        hookrun.PostCommit,
}

// hookHandler resolves an event name to its handler.
func hookHandler(event string) (adapter.Handler, bool) {
	if h, ok := gitHookEvents[event]; ok {
		return h, true
	}
	h, ok := adapter.Handlers()[event]
	return h, ok
}

// flushesAfter reports the events that kick off a detached spool flush afterwards: the
// natural moments when new events exist and a few hundred milliseconds of background
// work is invisible. Each adapter names its end-of-turn events; a throttle here can
// strand the final turn until another hook fires, so there is none. Sender backoff
// still applies. prepare-commit-msg never flushes — it has a 50 ms budget.
func flushesAfter(event string) bool {
	return event == "post-commit" || adapter.FlushesAfter(event)
}

// HooksDisabled reports the developer's kill switch: `TERMA_HOOKS=0` in the
// environment turns every hook into an immediate exit 0 — nothing is stamped,
// recorded or spooled — on a machine that has terma installed. It is the same
// convention as `HUSKY=0` and `LEFTHOOK=0`. A machine without terma needs no
// switch: every committed hook line is guarded so the commit proceeds untouched.
func HooksDisabled() bool {
	return os.Getenv("TERMA_HOOKS") == "0"
}

func newHookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook <event> [args...]",
		Short:  "Internal: the runtime behind every installed hook shim",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		// Hooks must exit 0 whatever happens; the shims add `|| true` as well, but
		// belt and braces is the right posture for something that runs on every
		// commit of every developer.
		RunE: func(cmd *cobra.Command, args []string) error {
			event := args[0]
			// The status line draws something, so it is the one hook the kill switch
			// must not silence: with TERMA_HOOKS=0 it still renders and merely does
			// not capture. Its exit status is the renderer's, so it ends the process
			// itself rather than returning through cobra.
			if event == "statusline" {
				os.Exit(runStatusLine(cmd, HooksDisabled()))
			}
			if HooksDisabled() {
				// terma replaced Codex's direct `notify` invocation, so even with capture
				// off the user's preserved notifier must still fire — the same carve-out
				// the status line gets above.
				if event == "codex-notify" && len(args) > 1 {
					ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
					defer cancel()
					_ = harness.RunPreviousCodexNotify(ctx, args[1])
				}
				return nil
			}
			handler, ok := hookHandler(event)
			if !ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "terma hook: unknown event %q (ignored)\n", event)
				return nil
			}
			cwd, err := os.Getwd()
			if err != nil {
				return nil
			}
			// Stdout is the hook's reply to the agent. Most handlers write nothing
			// there — Claude Code feeds SessionStart's stdout to the model — and the
			// ones that must (agy expects `{}`) do so themselves.
			env := hookrun.Env{
				Now:     time.Now(),
				Cwd:     cwd,
				Args:    args[1:],
				Stdin:   cmd.InOrStdin(),
				Stdout:  cmd.OutOrStdout(),
				Stderr:  cmd.ErrOrStderr(),
				Version: Version,
				Debug:   os.Getenv("TERMA_DEBUG") != "",
				Spool:   openSpool(),
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			_ = handler(ctx, env)
			if flushesAfter(event) && env.Spool != nil {
				spawnFlush()
			}
			return nil
		},
	}
	return cmd
}

// runStatusLine is `terma hook statusline`: Claude Code's statusLine command
// once Terma has wrapped it. The renderer has its own deadline even when Claude
// does not cancel it; capture starts detached delivery before waiting for rendering.
func runStatusLine(cmd *cobra.Command, captureDisabled bool) int {
	cwd, _ := os.Getwd()
	env := hookrun.Env{
		Now:     time.Now(),
		Cwd:     cwd,
		Stdin:   cmd.InOrStdin(),
		Stdout:  cmd.OutOrStdout(),
		Stderr:  cmd.ErrOrStderr(),
		Version: Version,
		Debug:   os.Getenv("TERMA_DEBUG") != "",
	}
	if !captureDisabled {
		env.Spool = openSpool()
	}
	renderer, err := harness.StatusLineRenderer()
	if err != nil && env.Debug {
		fmt.Fprintf(env.Stderr, "terma hook: status line record: %v\n", err)
	}
	return hookrun.StatusLine(cmd.Context(), env, hookrun.StatusLineOptions{Renderer: renderer, Indicator: !captureDisabled, OnCapture: func() { spawnFlush() }})
}

// openSpool returns the machine spool, or nil when the config dir cannot be used.
// A nil spool drops events; it never fails a hook.
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

// spawnFlush starts `terma spool flush --quiet` detached from the hook, so the
// hook returns immediately and the network happens in the background. Failures
// are silent: the next flush picks up whatever this one leaves.
func spawnFlush() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	proc := exec.Command(exe, "spool", "flush", "--quiet")
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	detach(proc)
	if err := proc.Start(); err != nil {
		return
	}
	_ = proc.Process.Release()
}
