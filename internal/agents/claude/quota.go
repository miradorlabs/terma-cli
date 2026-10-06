package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// quotaWindows are the status line's rate-limit windows and the keys each one leaves under.
var quotaWindows = []struct{ name, used, resets string }{
	{"five_hour", semconv.TermaRateLimitFiveHourUsedPercentKey, semconv.TermaRateLimitFiveHourResetsAtKey},
	{"seven_day", semconv.TermaRateLimitSevenDayUsedPercentKey, semconv.TermaRateLimitSevenDayResetsAtKey},
	{"spend_limit", semconv.TermaRateLimitSpendLimitUsedPercentKey, semconv.TermaRateLimitSpendLimitResetsAtKey},
}

// quotaState is the last emitted snapshot, per session, so a 300 ms redraw cadence becomes one
// event per change.
type quotaState struct {
	Stream    string             `json:"stream,omitempty"`
	Sequence  uint64             `json:"sequence,omitempty"`
	ProjectID string             `json:"project_id,omitempty"`
	EmittedAt time.Time          `json:"emitted_at"`
	Quota     map[string]float64 `json:"quota"`
	Model     string             `json:"model"`
	Resets    map[string]int64   `json:"resets"`
	AccountID string             `json:"account_id,omitempty"`
	OrgID     string             `json:"organization_id,omitempty"`
}

// captureQuota spools terma.session.quota when the snapshot changed or the heartbeat is due;
// empty startup redraws before any evidence are suppressed.
func captureQuota(e hookrun.Env, p *statusLinePayload) bool {
	if e.Spool == nil {
		return false
	}
	e.Cwd = cmp.Or(p.Cwd, e.Cwd)
	r, err := e.Repo(context.Background())
	if err != nil {
		e.Logf("quota: %v", err)
		return false
	}
	quota := map[string]float64{}
	resets := map[string]int64{}
	for _, w := range quotaWindows {
		if rl, ok := p.RateLimits[w.name]; ok && rl.UsedPercentage != nil {
			quota[w.name] = *rl.UsedPercentage
			if rl.ResetsAt != nil {
				resets[w.name] = *rl.ResetsAt
			}
		}
	}

	now := e.Time()
	statePath := quotaStatePath(e.StateDir, p.SessionID)
	if os.MkdirAll(filepath.Dir(statePath), 0o700) != nil {
		return false
	}
	unlock, err := hookrun.LockEvidence(statePath + ".lock")
	if err != nil {
		e.Logf("quota lock: %v", err)
		return false
	}
	defer unlock()
	prev := readQuotaState(statePath)
	if len(quota) == 0 && prev == nil {
		return false
	}
	projectID := r.ProjectID
	accountID, orgID, _ := sessionFunding(e, r.Root, p.SessionID, p.TranscriptPath, false).oauthAccount()
	next := quotaState{EmittedAt: now, Quota: quota, Model: p.Model.ID, Resets: resets, ProjectID: projectID, AccountID: accountID, OrgID: orgID}
	if prev != nil && !quotaChanged(*prev, next) && now.Sub(prev.EmittedAt) < hookrun.QuotaHeartbeat {
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
	// The snapshot is in the ID, so a later observation never collides after a failed checkpoint save.
	identity, _ := json.Marshal(next)
	attrs := map[string]any{
		semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaEvidenceSourceKey: sourceClaudeStatusline,
		semconv.TermaObservationStreamKey: next.Stream, semconv.TermaObservationSequenceKey: next.Sequence,
		semconv.TermaObservationIDKey:  hookrun.EvidenceID(p.SessionID + string(identity)),
		semconv.TermaEvidenceStatusKey: hookrun.StatusPresent,
	}
	if len(quota) == 0 {
		attrs[semconv.TermaEvidenceStatusKey] = hookrun.StatusUnavailable
	}
	if next.AccountID != "" {
		attrs[semconv.TermaAccountIDKey] = next.AccountID
	}
	if next.OrgID != "" {
		attrs[semconv.TermaAccountOrganizationIDKey] = next.OrgID
	}
	if p.Model.ID != "" {
		attrs[semconv.GenAIRequestModelKey] = p.Model.ID
	}
	for _, w := range quotaWindows {
		if pct, ok := quota[w.name]; ok {
			attrs[w.used] = pct
		}
		if at, ok := resets[w.name]; ok {
			attrs[w.resets] = at
		}
	}
	ev := spool.Event{Name: semconv.TermaSessionQuotaEvent, SessionID: p.SessionID, Repository: r.Repository, Attrs: attrs}
	if projectID != "" {
		attrs[hookrun.AttrProjectID] = projectID
	}
	ev.Time = now
	if err := e.Spool.Append(ev); err != nil {
		e.Logf("quota append: %v", err)
		return false
	}
	writeQuotaState(statePath, next)
	return true
}

func quotaChanged(a, b quotaState) bool {
	if a.ProjectID != b.ProjectID || a.AccountID != b.AccountID || a.OrgID != b.OrgID || !maps.Equal(a.Resets, b.Resets) ||
		a.Model != b.Model || len(a.Quota) != len(b.Quota) {
		return true
	}
	for k, v := range b.Quota {
		if pv, ok := a.Quota[k]; !ok || pv != v {
			return true
		}
	}
	return false
}

// quotaStatePath, under dir, hashes the session id: it is an identifier an agent chose, not a path.
func quotaStatePath(dir, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(dir, statusLineStateDir, hex.EncodeToString(sum[:])[:16]+".json")
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

// writeQuotaState persists the snapshot and, for a new session, prunes stale state files.
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
	_ = hookrun.WriteState(path, data)
	if fresh {
		hookrun.PruneState(dir, s.EmittedAt.Add(-hookrun.SnapshotStateRetention))
	}
}
