package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
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
// prompt, so it travels under repliesConsented, and only once captureCodexReplies reports
// every turn collected: any turn may have named it.
func captureCodexTitle(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput, collected bool) {
	if !collected || e.Spool == nil || !session.ValidID(in.SessionID) || !repliesConsented(e.Consent()) {
		return
	}
	dir := filepath.Join(e.StateDir, codexTitleStateDir)
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
	title, found, err := readThreadTitle(ctx, in.SessionID)
	if err != nil {
		e.Logf("codex title: %v", err)
		return
	}
	if !found || title.UpdatedAt.Equal(last.UpdatedAt) {
		return
	}
	attrs := map[string]any{
		semconv.GenAIMainAgentNameKey: codexTool, semconv.TermaEvidenceSourceKey: sourceCodexSessionIndex,
		semconv.TermaSessionTitleKey: truncateRunes(title.Name, codexTitleMaxText), hookrun.AttrProjectID: r.ProjectID,
	}
	at := e.Time()
	if !title.UpdatedAt.IsZero() && !title.UpdatedAt.After(at) {
		at = title.UpdatedAt
	}
	if err := e.Spool.Append(spool.Event{Time: at, Name: semconv.TermaSessionTitleEvent, SessionID: in.SessionID, Repository: r.Repository, Global: e.Policy.Global(), Attrs: attrs}); err != nil {
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
