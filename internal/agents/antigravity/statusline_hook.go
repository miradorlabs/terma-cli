package antigravity

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/statusline"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The status line hook captures the payload's plan, quota buckets and email as
// terma.session.quota and terma.session.account, each when it changed or the heartbeat is
// due, and runs the renderer it wrapped with the same bytes. Capture never delays
// rendering: the renderer starts first. agy runs it only in its interactive CLI, so a
// headless `agy -p` session reports no plan or quota.

// sourceAntigravityStatusline is terma.evidence.source for what agy's status line reported.
const sourceAntigravityStatusline = "antigravity_statusline"

// antigravitySeatScope scopes the seat id of an account agy names by email alone.
const antigravitySeatScope = "google"

// antigravityStatusLineDir holds the last quota snapshot each conversation spooled.
var antigravityStatusLineDir = hookrun.AgentStateDir(Agent{}.Name(), "statusline")

const (
	antigravityStatusLineMaxInput = 1 << 20
	// agy stops a status line command after five seconds; the renderer gets most of it.
	antigravityRendererTimeout = 4 * time.Second
	// antigravityMaxBuckets bounds the buckets one snapshot carries.
	antigravityMaxBuckets = 32
)

// antigravityStatusLinePayload is the allowlisted, content-free subset of the payload.
type antigravityStatusLinePayload struct {
	ConversationID string                     `json:"conversation_id"`
	SessionID      string                     `json:"session_id"`
	Cwd            string                     `json:"cwd"`
	PlanTier       string                     `json:"plan_tier"`
	Email          string                     `json:"email"`
	Quota          map[string]json.RawMessage `json:"quota"`
}

// id is the conversation the hooks name; agy's session_id is the CLI process's.
func (p *antigravityStatusLinePayload) id() string {
	return cmp.Or(p.ConversationID, p.SessionID)
}

// antigravityBucket is one quota bucket as agy reports it.
type antigravityBucket struct {
	RemainingFraction *float64        `json:"remaining_fraction"`
	RemainingAmount   json.RawMessage `json:"remaining_amount"`
	ResetTime         string          `json:"reset_time"`
	Disabled          *bool           `json:"disabled"`
}

// antigravityStatusLine runs the hook and returns the renderer's exit status, or 0.
func antigravityStatusLine(ctx context.Context, env hookrun.Env, renderer, shell string) int {
	stdout, stderr := env.Stdout, env.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	head, rest := statusline.ReadHead(env.Stdin, antigravityStatusLineMaxInput)
	if isAntigravityStatusLineOurs(renderer) {
		// Wrapping terma's own command again would loop.
		env.Logf("status line renderer calls terma itself; ignoring it")
		renderer = ""
	}
	var renderCtx context.Context
	var render *exec.Cmd
	var done chan error
	if strings.TrimSpace(renderer) != "" {
		var cancel context.CancelFunc
		renderCtx, cancel = context.WithTimeout(ctx, antigravityRendererTimeout)
		defer cancel()
		render, done = statusline.Start(renderCtx, renderer, shell, env.Cwd, head, rest, stdout, stderr)
	}
	if rest == nil {
		var p antigravityStatusLinePayload
		if err := json.Unmarshal(head, &p); err == nil && session.ValidID(p.id()) {
			if captureAntigravitySeat(env, &p) && env.Flush != nil {
				env.Flush()
			}
		} else {
			env.Logf("status line payload not captured: %v", err)
		}
	}
	if render == nil {
		// No renderer, or one that did not start: agy draws its own line.
		return 0
	}
	code := statusline.Wait(render, done)
	if errors.Is(renderCtx.Err(), context.DeadlineExceeded) {
		return 124
	}
	if code < 0 {
		return 1
	}
	return code
}

// antigravitySeatState is the last snapshot spooled for a conversation.
type antigravitySeatState struct {
	Stream    string    `json:"stream,omitempty"`
	Sequence  uint64    `json:"sequence,omitempty"`
	EmittedAt time.Time `json:"emitted_at"`
	// Hash covers what the snapshot reported, the email only as part of it.
	Hash string `json:"hash"`
}

// antigravityBuckets is the payload's quota as terma.rate_limit.buckets, in id order, with
// agy's ids and words as they came.
func antigravityBuckets(quota map[string]json.RawMessage) []map[string]any {
	ids := slices.Sorted(maps.Keys(quota))
	var out []map[string]any
	for _, id := range ids {
		if len(out) == antigravityMaxBuckets {
			break
		}
		var b antigravityBucket
		if !hookrun.ShortLabel(id) || json.Unmarshal(quota[id], &b) != nil {
			continue
		}
		m := map[string]any{"id": id}
		if f := b.RemainingFraction; f != nil && *f >= 0 && *f <= 1 {
			m["remaining_fraction"] = *f
		}
		if amount := antigravityAmount(b.RemainingAmount); amount != "" {
			m["remaining_amount"] = amount
		}
		if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
			m["resets_at"] = t.Unix()
		}
		if b.Disabled != nil {
			m["disabled"] = *b.Disabled
		}
		out = append(out, m)
	}
	return out
}

// antigravityAmount is a remaining amount as text, a number or string agy sent, kept exact.
func antigravityAmount(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if hookrun.ShortLabel(s) {
			return s
		}
		return ""
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if _, err := strconv.ParseFloat(n.String(), 64); err == nil {
			return n.String()
		}
	}
	return ""
}

// captureAntigravitySeat spools the plan, quota and account the payload names, when they
// changed or the heartbeat is due; empty redraws before any evidence are suppressed. It
// reports whether it queued anything.
func captureAntigravitySeat(e hookrun.Env, p *antigravityStatusLinePayload) bool {
	if e.Spool == nil {
		return false
	}
	e.Cwd = cmp.Or(p.Cwd, e.Cwd)
	r, err := e.Repo(context.Background())
	if err != nil {
		e.Logf("antigravity seat: %v", err)
		return false
	}
	id := p.id()
	account := map[string]any{}
	hookrun.BoundedAttr(account, semconv.TermaAccountPlanTypeKey, p.PlanTier)
	if hookrun.ValidEmail(p.Email) {
		// Delivery withholds the email with content; the seat id stays in its place.
		account[semconv.UserEmailKey] = p.Email
		account[semconv.TermaAccountSeatIDKey] = shape.SeatID(antigravitySeatScope, p.Email)
	}
	buckets := antigravityBuckets(p.Quota)
	if len(account) > 0 {
		status := hookrun.StatusPresent
		e.CaptureFunding(r, id, antigravityTool, semconv.TermaSessionAccountEvent, hookrun.FundingEvidence{
			Source: sourceAntigravityStatusline, Status: status, Attrs: maps.Clone(account),
		})
	}

	now := e.Time()
	path := filepath.Join(e.StateDir, antigravityStatusLineDir, hookrun.EvidenceID(id)+".json")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return false
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return false
	}
	defer unlock()
	var prev *antigravitySeatState
	if b, err := os.ReadFile(path); err == nil {
		var s antigravitySeatState
		if json.Unmarshal(b, &s) == nil {
			prev = &s
		}
	}
	if len(buckets) == 0 && prev == nil {
		return false
	}
	snapshot, _ := json.Marshal(map[string]any{"buckets": buckets, "account": account, "project": r.ProjectID})
	next := antigravitySeatState{EmittedAt: now, Hash: hookrun.EvidenceID(string(snapshot)), Sequence: 1}
	if prev != nil && prev.Hash == next.Hash && !now.Before(prev.EmittedAt) && now.Sub(prev.EmittedAt) < hookrun.QuotaHeartbeat {
		return false
	}
	if prev != nil {
		next.Stream, next.Sequence = prev.Stream, prev.Sequence+1
	}
	if next.Stream == "" {
		next.Stream = rand.Text()
	}
	attrs := map[string]any{
		semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaEvidenceSourceKey: sourceAntigravityStatusline,
		semconv.TermaEvidenceStatusKey:    hookrun.StatusPresent,
		semconv.TermaObservationStreamKey: next.Stream, semconv.TermaObservationSequenceKey: next.Sequence,
		semconv.TermaObservationIDKey: hookrun.EvidenceID(id + "\x00" + next.Stream + "\x00" + strconv.FormatUint(next.Sequence, 10) + "\x00" + next.Hash),
	}
	maps.Copy(attrs, account)
	if len(buckets) == 0 {
		attrs[semconv.TermaEvidenceStatusKey] = hookrun.StatusUnavailable
	} else {
		list := make([]any, len(buckets))
		for i, b := range buckets {
			list[i] = b
		}
		attrs[semconv.TermaRateLimitBucketsKey] = list
	}
	if r.ProjectID != "" {
		attrs[hookrun.AttrProjectID] = r.ProjectID
	}
	if err := e.Spool.Append(spool.Event{Time: now, Name: semconv.TermaSessionQuotaEvent, SessionID: id, Repository: r.Repository, Global: e.Policy.Global(), Attrs: attrs}); err != nil {
		e.Logf("antigravity quota append: %v", err)
		return false
	}
	b, _ := json.Marshal(next)
	fresh := prev == nil
	if err := hookrun.WriteState(path, b); err == nil && fresh {
		hookrun.PruneState(filepath.Dir(path), now.Add(-hookrun.SnapshotStateRetention))
	}
	return true
}
