package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// statusLineStateDir holds the last quota snapshot each session spooled.
const statusLineStateDir = "statusline"

// The status line hook both captures the payload (the plan's `rate_limits`, fast mode, the running
// estimate) as terma.session.quota and runs the renderer it wrapped with the same bytes, shell,
// environment and directory. Capture never delays rendering: the renderer starts first.

// statusLineOptions configures `terma hook statusline`.
type statusLineOptions struct {
	// RendererTimeout overrides TERMA_STATUSLINE_TIMEOUT and the 30s default; nonpositive keeps them.
	RendererTimeout time.Duration
	// Renderer is the command configured before terma's, run with `sh -c`; empty means none.
	Renderer string
	// Shell overrides the renderer's shell, for tests.
	Shell       string
	CaptureOnly bool
	// Indicator prefixes the first rendered line with terma's mark; off when capture is off, since
	// the mark means "watching", not "installed".
	Indicator bool
	// OnCapture starts delivery after a new snapshot is queued: rendering can happen after Stop flushed.
	OnCapture func()
}

func indicatorMark() string {
	if seq := style.BrandSequence(); seq != "" {
		return seq + "t" + style.Reset + " "
	}
	return "t "
}

// statusLineMaxInput bounds what is parsed; a larger payload still reaches the renderer whole.
const statusLineMaxInput = 1 << 20

const defaultRendererTimeout = 30 * time.Second

// rendererPipeDrain bounds the wait for output pipes after the shell exits: a descendant can hold them.
const rendererPipeDrain = 100 * time.Millisecond

func statusLineRendererTimeout(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if timeout, err := time.ParseDuration(os.Getenv("TERMA_STATUSLINE_TIMEOUT")); err == nil && timeout > 0 {
		return timeout
	}
	return defaultRendererTimeout
}

// statusLinePayload is the allowlisted, content-free subset of the payload.
type statusLinePayload struct {
	SessionID string `json:"session_id"`
	PromptID  string `json:"prompt_id"`
	Version   string `json:"version"`
	Cwd       string `json:"cwd"`
	Model     struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	FastMode *bool `json:"fast_mode"`
	Cost     struct {
		TotalCostUSD *float64 `json:"total_cost_usd"`
	} `json:"cost"`
	ContextWindow struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits map[string]struct {
		UsedPercentage *float64 `json:"used_percentage"`
		ResetsAt       *int64   `json:"resets_at"`
	} `json:"rate_limits"`
}

// statusLine runs the hook and returns the renderer's exit status, or 0.
func statusLine(ctx context.Context, env hookrun.Env, opts statusLineOptions) int {
	stdout := env.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := env.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	head, rest := readHead(env.Stdin, statusLineMaxInput)

	// Wrapping terma's own command again would loop.
	renderer := opts.Renderer
	if strings.TrimSpace(renderer) == "" {
		renderer = ""
	}
	if isStatusLineCommand(renderer) {
		env.Logf("status line renderer calls terma itself; ignoring it")
		renderer = ""
	}

	// With the indicator on, the renderer draws into a buffer so the mark can lead its first line.
	var render *exec.Cmd
	var renderDone chan error
	var renderCtx context.Context
	var drawn bytes.Buffer
	renderOut := stdout
	if opts.Indicator {
		renderOut = &drawn
	}
	if renderer != "" && !opts.CaptureOnly {
		var cancel context.CancelFunc
		renderCtx, cancel = context.WithTimeout(ctx, statusLineRendererTimeout(opts.RendererTimeout))
		defer cancel()
		render, renderDone = startRenderer(renderCtx, renderer, opts.Shell, env.Cwd, head, rest, renderOut, stderr)
	}

	var payload *statusLinePayload
	if rest == nil {
		var p statusLinePayload
		if err := json.Unmarshal(head, &p); err == nil && p.SessionID != "" {
			payload = &p
		} else {
			env.Logf("status line payload not captured: %v", err)
		}
	}
	if payload != nil {
		if captureQuota(env, payload) && opts.OnCapture != nil {
			opts.OnCapture()
		}
	}

	if render != nil {
		code := waitRenderer(render, renderDone)
		if opts.Indicator {
			_, _ = stdout.Write(withIndicator(drawn.Bytes()))
		}
		if errors.Is(renderCtx.Err(), context.DeadlineExceeded) {
			return 124
		}
		if code < 0 {
			return 1
		}
		return code
	}
	if renderer != "" && !opts.CaptureOnly {
		return 1 // The renderer could not be started; capture still ran.
	}
	return 0
}

// withIndicator puts the mark in front of a rendering; an empty one stays empty so a renderer can
// hide the status line.
func withIndicator(out []byte) []byte {
	mark := indicatorMark()
	if len(out) == 0 {
		return out
	}
	return append([]byte(mark), out...)
}

// readHead reads up to limit bytes; when the input is longer, rest continues after head.
func readHead(r io.Reader, limit int) (head []byte, rest io.Reader) {
	if r == nil {
		return nil, nil
	}
	head, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return head, nil
	}
	if len(head) > limit {
		return head, r
	}
	return head, nil
}

// startRenderer runs the previous command as Claude Code would, in its own process group on Unix
// so a cancellation (Claude Code kills an in-flight command on a newer update) reaches it.
func startRenderer(ctx context.Context, command, shell, cwd string, head []byte, rest io.Reader, stdout, stderr io.Writer) (*exec.Cmd, chan error) {
	if shell == "" {
		shell = "sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureRendererProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "terma: status line renderer did not start: %v\n", err)
		return nil, nil
	}
	go func() {
		defer stdin.Close()
		if _, err := stdin.Write(head); err != nil {
			return
		}
		if rest != nil {
			_, _ = io.Copy(stdin, rest)
		}
	}()
	// exited only signals the exit, so the signal forwarder stops without consuming done.
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if errors.Is(err, exec.ErrWaitDelay) {
			// Descendants kept the pipes open: stop the group too.
			_ = cmd.Cancel()
		}
		done <- err
		close(exited)
	}()

	forwardRendererSignals(cmd, exited)
	return cmd, done
}

func waitRenderer(cmd *exec.Cmd, done chan error) int {
	err := <-done
	if err == nil {
		return 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
		return rendererSignalExitCode(exit)
	}
	return -1
}
