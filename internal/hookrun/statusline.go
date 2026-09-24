package hookrun

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// The status line is the one hook that draws something. Claude Code runs the
// configured command with the session's JSON on stdin every time the display
// changes, debounced at 300 ms, and shows whatever it prints. Terma installs its
// own command in front of whatever was configured, so `terma hook statusline`
// has two jobs that must not interfere:
//
//   - capture: the payload carries the provider's own view of the seat's
//     rate-limit windows (`rate_limits`, present on Pro, Max and Team after the
//     first response), the fast-mode switch and the session's running estimate.
//     That is the strongest funding evidence a machine can produce, and it is
//     spooled as terma.session.quota when it changes.
//   - render: the renderer that was configured before Terma wrapped it runs
//     exactly as it would have — same bytes on stdin, same shell, same
//     environment and working directory, stdout and stderr connected straight
//     through, its exit status returned — so nothing anyone built or installed
//     for their status line changes. Without a previous renderer, capture is
//     silent: no default line and no indicator are drawn.
//
// The budget is the same as every other hook: no network, no git subprocess,
// one small state file. Capture never delays or alters rendering: the renderer
// is started first and the capture runs while it draws.

// StatusLineOptions is how `terma hook statusline` is configured by the
// installer's record of what it wrapped.
type StatusLineOptions struct {
	// RendererTimeout overrides TERMA_STATUSLINE_TIMEOUT and the 30s default.
	// Nonpositive values use the environment/default; there is no unlimited mode.
	RendererTimeout time.Duration
	// Renderer is the status line command configured before Terma's, run with
	// `sh -c` as Claude Code runs it. Empty means there was none.
	Renderer string
	// Shell overrides the shell the renderer runs in (tests). Default: sh.
	Shell string
	// CaptureOnly skips rendering entirely (tests and diagnostics).
	CaptureOnly bool
	// Indicator prefixes the first rendered line with Terma's mark, so a glance at
	// the status line says the session is being watched. Off when capture is off:
	// the mark means "watching", not "installed".
	Indicator bool
	// OnCapture starts delivery after a new snapshot is safely queued. Claude
	// may render the status line after Stop has already flushed the spool.
	OnCapture func()
}

// indicatorMark is the mark itself: a lowercase t in the brand colour, then a space.
func indicatorMark() string {
	if seq := style.BrandSequence(); seq != "" {
		return seq + "t" + style.Reset + " "
	}
	return "t "
}

// statusLineMaxInput bounds what is parsed. Claude's payload is a few kilobytes;
// anything larger is passed to the renderer untouched and simply not captured.
const statusLineMaxInput = 1 << 20

const defaultRendererTimeout = 30 * time.Second

// rendererPipeDrain bounds the wait for the renderer's output pipes once its shell has
// exited or been cancelled: a descendant can keep them open long after.
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

// quotaHeartbeat is how often an unchanged snapshot is re-sent, so the backend
// can tell "no change" from "no status line".
const quotaHeartbeat = 10 * time.Minute

// statusLinePayload is the allowlisted subset of what Claude Code writes. Every
// other field is ignored; nothing here is content.
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

// quotaWindows are the rate_limits keys Claude Code documents.
var quotaWindows = []string{"five_hour", "seven_day", "spend_limit"}

// quotaState is what the last emitted snapshot looked like, kept per session so
// the 300 ms cadence of the status line turns into one event per change.
type quotaState struct {
	Stream    string             `json:"stream,omitempty"`
	Sequence  uint64             `json:"sequence,omitempty"`
	ProjectID string             `json:"project_id,omitempty"`
	EmittedAt time.Time          `json:"emitted_at"`
	Quota     map[string]float64 `json:"quota"`
	FastMode  *bool              `json:"fast_mode,omitempty"`
	Model     string             `json:"model"`
	PromptID  string             `json:"prompt_id"`
	Resets    map[string]int64   `json:"resets"`
	Cost      *float64           `json:"cost,omitempty"`
	AccountID string             `json:"account_id,omitempty"`
}

// StatusLine runs the status line hook and returns the exit status to end with:
// the renderer's own, or 0.
func StatusLine(ctx context.Context, env Env, opts StatusLineOptions) int {
	stdout := env.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := env.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	head, rest := readHead(env.Stdin, statusLineMaxInput)

	// Wrapping Terma's own command a second time would loop; treat it as no renderer.
	renderer := opts.Renderer
	if strings.TrimSpace(renderer) == "" {
		renderer = ""
	}
	if harness.IsStatusLineCommand(renderer) {
		env.logf("status line renderer calls terma itself; ignoring it")
		renderer = ""
	}

	// With the indicator on, the renderer draws into a buffer so the mark can go
	// in front of its first line; otherwise its stdout is connected straight
	// through. Either way its bytes are never altered.
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
			env.logf("status line payload not captured: %v", err)
		}
	}
	if payload != nil {
		if env.captureQuota(payload) && opts.OnCapture != nil {
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

// withIndicator puts the mark in front of the first line of a rendering. An
// empty rendering stays empty: conditional renderers must remain able to hide
// their status line. Everything after the marker is byte-for-byte unchanged.
func withIndicator(out []byte) []byte {
	mark := indicatorMark()
	if len(out) == 0 {
		return out
	}
	return append([]byte(mark), out...)
}

// readHead reads up to limit bytes. When the input is longer, rest is the reader
// positioned after head, so a renderer still receives every byte.
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

// startRenderer runs the previous status line command the way Claude Code
// would have: through the shell, in the same directory and environment, with
// the payload on stdin and its output connected straight through. It runs in
// its own process group on Unix so that a cancellation from Claude Code (which kills
// the in-flight command when a newer update arrives) reaches it as well.
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
		// The shell itself could not start. sh prints "command not found" for a
		// missing renderer on its own; this is rarer and worth a line.
		fmt.Fprintf(stderr, "terma: status line renderer did not start: %v\n", err)
		return nil, nil
	}
	go func() {
		// A renderer that never reads stdin closes the pipe on us; that is fine.
		defer stdin.Close()
		if _, err := stdin.Write(head); err != nil {
			return
		}
		if rest != nil {
			_, _ = io.Copy(stdin, rest)
		}
	}()
	// done carries the exit to the one waiter; exited only signals that it
	// happened, so the signal forwarder below can stop without consuming it.
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if errors.Is(err, exec.ErrWaitDelay) {
			// The shell exited but descendants kept its output pipes open. Stop
			// the group too, rather than merely closing our copies of the pipes.
			_ = cmd.Cancel()
		}
		done <- err
		close(exited)
	}()

	forwardRendererSignals(cmd, exited)
	return cmd, done
}

// waitRenderer returns the renderer's exit code, or -1 when it did not run to
// an exit status.
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

// captureQuota spools one terma.session.quota event when the snapshot changed
// or the heartbeat is due. Missing windows supersede previous quota evidence.
// Empty startup redraws before any evidence are suppressed.
func (e Env) captureQuota(p *statusLinePayload) bool {
	if e.Spool == nil {
		return false
	}
	quota := map[string]float64{}
	resets := map[string]int64{}
	for _, w := range quotaWindows {
		if rl, ok := p.RateLimits[w]; ok && rl.UsedPercentage != nil {
			quota[w] = *rl.UsedPercentage
			if rl.ResetsAt != nil {
				resets[w] = *rl.ResetsAt
			}
		}
	}

	now := e.now()
	statePath, err := quotaStatePath(p.SessionID)
	if err != nil {
		e.logf("quota state: %v", err)
		return false
	}
	if os.MkdirAll(filepath.Dir(statePath), 0o700) != nil {
		return false
	}
	unlock, err := lockEvidence(statePath + ".lock")
	if err != nil {
		e.logf("quota lock: %v", err)
		return false
	}
	defer unlock()
	prev := readQuotaState(statePath)
	// Preserve disappearance of windows after an observed snapshot, but avoid
	// emitting empty startup redraws before any funding evidence was available.
	if len(quota) == 0 && p.FastMode == nil && prev == nil {
		return false
	}
	repo, worktree, projectID, repoRoot := "", "", "", ""
	root, gitDir, located := gitx.LocateFS(cmp.Or(p.Cwd, e.Cwd))
	if !located {
		// A workspace outside Git (PR #4) is found by its binding.
		root, _ = project.Find(cmp.Or(p.Cwd, e.Cwd))
	}
	if root != "" {
		repoRoot = root
		repo, worktree = checkoutNames(root, gitDir)
		if f, _, err := project.Resolve(root, gitDir); err == nil {
			projectID = f.Project.ID
		}
	}
	accountID, _ := claudeOAuthAccountID(repoRoot)
	next := quotaState{EmittedAt: now, Quota: quota, FastMode: p.FastMode, Model: p.Model.ID,
		PromptID: p.PromptID, Resets: resets, Cost: p.Cost.TotalCostUSD, ProjectID: projectID, AccountID: accountID}
	if prev != nil && !quotaChanged(*prev, next) && now.Sub(prev.EmittedAt) < quotaHeartbeat {
		return false
	}

	next.Sequence = 1
	if prev != nil {
		next.Stream = prev.Stream
		next.Sequence = prev.Sequence + 1
	}
	if next.Stream == "" {
		next.Stream = rand.Text()
	}
	// Include the snapshot in the ID: if saving the checkpoint fails, a later
	// different observation must not collide with the previous sequence number.
	identity, _ := json.Marshal(next)
	attrs := map[string]any{
		attrTool: claudeTool, attrVersion: e.Version,
		attrEvidenceSource: "claude_statusline", attrSchemaVersion: 1,
		"source_stream": next.Stream, "observation_sequence": next.Sequence,
		"observation_id":   evidenceID(p.SessionID + string(identity)),
		attrEvidenceStatus: statusPresent, "time_basis": "observed",
	}
	if len(quota) == 0 {
		attrs[attrEvidenceStatus] = statusUnavailable
	}
	if next.AccountID != "" {
		attrs[attrAccountID] = next.AccountID
	}
	if p.Model.ID != "" {
		attrs[attrModel] = p.Model.ID
	}
	if p.Version != "" {
		attrs["claude.version"] = p.Version
	}
	if p.PromptID != "" {
		attrs["prompt_id"] = p.PromptID
	}
	if p.FastMode != nil {
		attrs["fast_mode"] = *p.FastMode
	}
	if p.Cost.TotalCostUSD != nil {
		attrs["session_cost_usd"] = *p.Cost.TotalCostUSD
	}
	for w, pct := range quota {
		attrs[w+"_used_pct"] = pct
		if at, ok := resets[w]; ok {
			attrs[w+"_resets_at"] = at
		}
	}
	ev := spool.Event{Name: EventSessionQuota, SessionID: p.SessionID, Repo: repo, Attrs: attrs}
	if worktree != "" {
		attrs[AttrWorktree] = worktree
	}
	if projectID != "" {
		attrs[AttrProjectID] = projectID
	}
	ev.Time = now
	if err := e.Spool.Append(ev); err != nil {
		e.logf("quota append: %v", err)
		return false
	}
	writeQuotaState(statePath, next)
	return true
}

func quotaChanged(a, b quotaState) bool {
	if a.ProjectID != b.ProjectID || a.PromptID != b.PromptID || a.AccountID != b.AccountID || !maps.Equal(a.Resets, b.Resets) ||
		(a.Cost == nil) != (b.Cost == nil) || (a.Cost != nil && b.Cost != nil && *a.Cost != *b.Cost) {
		return true
	}
	if a.Model != b.Model || len(a.Quota) != len(b.Quota) {
		return true
	}
	if (a.FastMode == nil) != (b.FastMode == nil) || (a.FastMode != nil && *a.FastMode != *b.FastMode) {
		return true
	}
	for k, v := range b.Quota {
		if pv, ok := a.Quota[k]; !ok || pv != v {
			return true
		}
	}
	return false
}

// quotaStatePath is the per-session state file under the config dir. The
// session id is hashed: it is an identifier a harness chose, not a path.
func quotaStatePath(sessionID string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(dir, statusLineStateDir, hex.EncodeToString(sum[:])[:16]+".json"), nil
}

func readQuotaState(path string) *quotaState {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s quotaState
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

// writeQuotaState persists the snapshot atomically and, when the session is
// new, drops state files no session has touched in two days.
func writeQuotaState(path string, s quotaState) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	fresh := false
	if _, err := os.Stat(path); err != nil {
		fresh = true
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = writeState(path, data)
	if fresh {
		pruneQuotaState(dir, s.EmittedAt.Add(-snapshotStateRetention))
	}
}

// pruneQuotaState ages out a state directory: every `<id>.json` last written before the
// cutoff, and then the `<id>.json.lock` beside it. The locks used to be left behind —
// one per session, for ever — because only the data files were matched.
//
// A lock goes only when all three hold: it is past the cutoff itself, its data file is
// gone, and nothing holds it (the prune takes it before unlinking). A lock's mtime is
// its creation, so a session that outlives the cutoff keeps its lock through its data
// file, which every write refreshes.
func pruneQuotaState(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var locks []string
	for _, ent := range entries {
		info, err := ent.Info()
		if err != nil || ent.IsDir() || !info.ModTime().Before(before) {
			continue
		}
		switch name := ent.Name(); {
		case strings.HasSuffix(name, ".json"):
			_ = os.Remove(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".json.lock"):
			locks = append(locks, filepath.Join(dir, name))
		}
	}
	for _, lock := range locks {
		if _, err := os.Lstat(strings.TrimSuffix(lock, ".lock")); !os.IsNotExist(err) {
			continue
		}
		unlock, err := lockEvidence(lock)
		if err != nil {
			continue
		}
		_ = os.Remove(lock)
		unlock()
	}
}
