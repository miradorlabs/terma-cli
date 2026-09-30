package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// codexTitleMaxText bounds a title. Codex generates at most 36 characters; a manual
// rename has no limit of its own.
const codexTitleMaxText = 256

// codexTitleState is the name a session's capture last spooled, so an unchanged name is
// not sent again at every turn's end.
type codexTitleState struct {
	UpdatedAt time.Time `json:"updated_at"`
}

// captureCodexTitle spools the name Codex gave this thread when it is new or renamed.
// The name restates the developer's first prompt, so it travels under the consent a
// reply does (codexRepliesConsented).
func (e Env) captureCodexTitle(ctx context.Context, r *repo, in *codexHookInput) {
	pol := routing.EffectivePolicy(e.Policy, r.projectID)
	if e.Spool == nil || !session.ValidID(in.SessionID) || !pol.IncludePrompts || !pol.AllowsSignal("logs") || len(pol.ExcludePaths) > 0 || !CodexRepliesConsented(r.projectID, pol.Global()) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, codexTitleStateDir)
	path := filepath.Join(dir, evidenceID(in.SessionID)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := lockEvidence(path + ".lock")
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
	title, found, err := harness.ReadCodexThreadTitle(ctx, in.SessionID)
	if err != nil {
		e.logf("codex title: %v", err)
		return
	}
	if !found || title.UpdatedAt.Equal(last.UpdatedAt) {
		return
	}
	attrs := map[string]any{
		attrTool: codexTool, attrSchemaVersion: 1, attrEvidenceSource: sourceCodexSessionIndex,
		"title": truncateRunes(title.Name, codexTitleMaxText), attrVersion: e.Version, AttrProjectID: r.projectID,
	}
	r.stampWorktree(attrs)
	at := e.now()
	if !title.UpdatedAt.IsZero() && !title.UpdatedAt.After(at) {
		at = title.UpdatedAt
	}
	if err := e.Spool.Append(spool.Event{Time: at, Name: EventSessionTitle, SessionID: in.SessionID, Repo: r.name, Workspace: r.root, Global: pol.Global(), Attrs: attrs}); err != nil {
		e.logf("codex title: %v", err)
		return
	}
	if b, err := json.Marshal(codexTitleState{UpdatedAt: title.UpdatedAt}); err == nil {
		if err := writeState(path, b); err != nil {
			e.logf("title state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		pruneState(dir, e.now().Add(-spool.MaxAge))
	}
}

// truncateRunes cuts s to at most n bytes on a rune boundary.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
