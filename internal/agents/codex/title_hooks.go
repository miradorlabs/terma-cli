package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// codexTitleMaxText bounds a title: Codex generates at most 36 characters, a rename has
// no limit.
const codexTitleMaxText = 256

// codexTitleState keeps an unchanged name from being sent at every turn's end.
type codexTitleState struct {
	UpdatedAt time.Time `json:"updated_at"`
}

// captureCodexTitle spools the thread's name when new or renamed; it restates the first
// prompt, so it travels under CodexRepliesConsented.
func captureCodexTitle(e hookrun.Env, ctx context.Context, r *hookrun.Repo, in *codexHookInput) {
	pol := routing.EffectivePolicy(e.Policy, r.ProjectID)
	if e.Spool == nil || !session.ValidID(in.SessionID) || !pol.IncludePrompts || !pol.AllowsSignal("logs") || len(pol.ExcludePaths) > 0 || !CodexRepliesConsented(r.ProjectID, pol.Global()) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, codexTitleStateDir)
	path := filepath.Join(dir, hookrun.EvidenceID(in.SessionID)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var last codexTitleState
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &last) != nil {
		last = codexTitleState{}
	}
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	title, found, err := ReadCodexThreadTitle(ctx, in.SessionID)
	if err != nil {
		e.Logf("codex title: %v", err)
		return
	}
	if !found || title.UpdatedAt.Equal(last.UpdatedAt) {
		return
	}
	attrs := map[string]any{
		hookrun.AttrTool: codexTool, hookrun.AttrSchemaVersion: 1, hookrun.AttrEvidenceSource: sourceCodexSessionIndex,
		"title": truncateRunes(title.Name, codexTitleMaxText), hookrun.AttrVersion: e.Version, hookrun.AttrProjectID: r.ProjectID,
	}
	r.StampWorktree(attrs)
	at := e.Time()
	if !title.UpdatedAt.IsZero() && !title.UpdatedAt.After(at) {
		at = title.UpdatedAt
	}
	if err := e.Spool.Append(spool.Event{Time: at, Name: hookrun.EventSessionTitle, SessionID: in.SessionID, Repo: r.Name, Workspace: r.Root, Global: pol.Global(), Attrs: attrs}); err != nil {
		e.Logf("codex title: %v", err)
		return
	}
	if b, err := json.Marshal(codexTitleState{UpdatedAt: title.UpdatedAt}); err == nil {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("title state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
