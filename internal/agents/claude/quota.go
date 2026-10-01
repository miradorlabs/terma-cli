package claude

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

var quotaWindows = []string{"five_hour", "seven_day", "spend_limit"}

// quotaState is the last emitted snapshot, per session, so a 300 ms redraw cadence becomes one
// event per change.
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
	OrgID     string             `json:"organization_id,omitempty"`
}

// captureQuota spools terma.session.quota when the snapshot changed or the heartbeat is due;
// empty startup redraws before any evidence are suppressed.
func captureQuota(e hookrun.Env, p *statusLinePayload) bool {
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

	now := e.Time()
	statePath, err := quotaStatePath(p.SessionID)
	if err != nil {
		e.Logf("quota state: %v", err)
		return false
	}
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
	if len(quota) == 0 && p.FastMode == nil && prev == nil {
		return false
	}
	repo, worktree, projectID, repoRoot := "", "", "", ""
	root, gitDir, located := gitx.LocateFS(cmp.Or(p.Cwd, e.Cwd))
	if !located {
		// A workspace outside Git is found by its binding.
		root, _ = project.Find(cmp.Or(p.Cwd, e.Cwd))
	}
	if root != "" {
		repoRoot = root
		repo, worktree = hookrun.CheckoutNames(root, gitDir)
		if f, _, err := project.Resolve(root, gitDir); err == nil {
			projectID = f.Project.ID
		}
	}
	accountID, orgID, _ := claudeOAuthAccount(repoRoot)
	next := quotaState{EmittedAt: now, Quota: quota, FastMode: p.FastMode, Model: p.Model.ID,
		PromptID: p.PromptID, Resets: resets, Cost: p.Cost.TotalCostUSD, ProjectID: projectID, AccountID: accountID, OrgID: orgID}
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
		hookrun.AttrTool: claudeTool, hookrun.AttrVersion: e.Version,
		hookrun.AttrEvidenceSource: "claude_statusline", hookrun.AttrSchemaVersion: 1,
		"source_stream": next.Stream, "observation_sequence": next.Sequence,
		"observation_id":           hookrun.EvidenceID(p.SessionID + string(identity)),
		hookrun.AttrEvidenceStatus: hookrun.StatusPresent, "time_basis": "observed",
	}
	if len(quota) == 0 {
		attrs[hookrun.AttrEvidenceStatus] = hookrun.StatusUnavailable
	}
	if next.AccountID != "" {
		attrs[hookrun.AttrAccountID] = next.AccountID
	}
	if next.OrgID != "" {
		attrs[hookrun.AttrOrganizationID] = next.OrgID
	}
	if p.Model.ID != "" {
		attrs[hookrun.AttrModel] = p.Model.ID
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
	ev := spool.Event{Name: hookrun.EventSessionQuota, SessionID: p.SessionID, Repo: repo, Attrs: attrs}
	if worktree != "" {
		attrs[hookrun.AttrWorktree] = worktree
	}
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
	if a.ProjectID != b.ProjectID || a.PromptID != b.PromptID || a.AccountID != b.AccountID || a.OrgID != b.OrgID || !maps.Equal(a.Resets, b.Resets) ||
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

// quotaStatePath hashes the session id: it is an identifier an agent chose, not a path.
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
