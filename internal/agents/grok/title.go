package grok

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// grok names a session from its conversation right after the first prompt, names it again at a
// couple of early turns, then keeps the name; a /rename replaces it for good. Its export carries
// no name: grok keeps it in summary.json (generated_title), beside the session's updates.jsonl,
// which every hook after SessionStart names as transcript_path. Each Stop and SessionEnd read it
// and spool a new or changed name as terma.session.title. The name restates the first prompt, so
// it travels under ContentConsented and, at delivery, the policy's prompt switch. A subagent's
// session is not titled.

// sourceGrokSummary is where a session's name comes from: grok's summary.json.
const sourceGrokSummary = "grok_summary"

var titleDir = hookrun.AgentStateDir(name, "titles")

// titled reports whether the hook's session can be titled: its own, with a summary to read.
func titled(in *grokHookInput) bool { return !in.child() && summaryPath(in) != "" }

// captureTitle spools the session's name, in its checkout r, when it is new or changed.
func captureTitle(env hookrun.Env, r *hookrun.Repo, in *grokHookInput) {
	if !titled(in) || !(Agent{}).ContentConsented(env.Consent(r)) {
		return
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: name, semconv.TermaEvidenceSourceKey: sourceGrokSummary}
	hookrun.SpoolTitle(env, r, titleDir, in.SessionID, attrs, func() (string, bool, error) {
		return readTitle(summaryPath(in))
	})
}

// summaryPath is the session's summary.json, beside the updates.jsonl the hook names when that
// is the session's own; "" otherwise.
func summaryPath(in *grokHookInput) string {
	p := in.TranscriptPath
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if filepath.Base(p) != "updates.jsonl" || filepath.Base(filepath.Dir(p)) != in.SessionID {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "summary.json")
}

// readTitle is the name summary.json at path holds, trimmed; found is false before grok has
// written the file or named the session.
func readTitle(path string) (title string, found bool, err error) {
	doc, status := hookrun.ReadEvidenceJSON(path)
	switch status {
	case "present":
	case "missing":
		return "", false, nil
	default:
		return "", false, errors.New("summary.json is " + status)
	}
	if raw, ok := doc["generated_title"]; ok {
		if err := json.Unmarshal(raw, &title); err != nil {
			return "", false, err
		}
	}
	title = strings.TrimSpace(title)
	return title, title != "", nil
}
