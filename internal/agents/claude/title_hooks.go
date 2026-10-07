package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

var claudeTitleStateDir = hookrun.AgentStateDir(name, "titles")

// claudeTitleMaxText bounds a title: Claude Code generates a short one, a rename has no limit.
const claudeTitleMaxText = 256

// claudeTitleState keeps an unchanged name from being sent at every turn's end.
type claudeTitleState struct {
	Title string `json:"title"`
}

// captureClaudeTitle spools the session's name when new or renamed. It restates the first
// prompt, so it travels under titleConsented and, at delivery, the policy's prompt switch.
func captureClaudeTitle(e hookrun.Env, r *hookrun.Repo, in *claudeHookInput) {
	if e.Spool == nil || in.TranscriptPath == "" || !session.ValidID(in.SessionID) || !titleConsented(e.Consent()) {
		return
	}
	dir := filepath.Join(e.StateDir, claudeTitleStateDir)
	path := filepath.Join(dir, hookrun.EvidenceID(in.SessionID)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var last claudeTitleState
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &last) != nil {
		last = claudeTitleState{}
	}
	title, found, err := readTranscriptTitle(in.TranscriptPath, in.SessionID)
	if err != nil {
		e.Logf("claude title: %v", err)
		return
	}
	title = truncateBytes(title, claudeTitleMaxText)
	if !found || title == last.Title {
		return
	}
	attrs := map[string]any{
		semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaEvidenceSourceKey: sourceClaudeTranscript,
		semconv.TermaSessionTitleKey: title, hookrun.AttrProjectID: r.ProjectID,
	}
	if err := e.Spool.Append(spool.Event{Time: e.Time(), Name: semconv.TermaSessionTitleEvent, SessionID: in.SessionID, Repository: r.Repository, Global: r.Policy.Global(), Attrs: attrs}); err != nil {
		e.Logf("claude title: %v", err)
		return
	}
	if b, err := json.Marshal(claudeTitleState{Title: title}); err == nil {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("title state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}

// titleConsented reports whether a title may be sent at all: Claude Code is among the
// developer's agents, which setup points at the relay, or global mode collects every
// session. The team's policy, checked at delivery, decides whether prompts may.
func titleConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, name)
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
