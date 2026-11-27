package grok

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
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

// summaryMaxBytes bounds the read of summary.json, which holds a few summaries beside the name.
const summaryMaxBytes = 1 << 20

var titleDir = hookrun.AgentStateDir(name, "titles")

// captureTitle spools the session's name when it is new or changed.
func captureTitle(ctx context.Context, env hookrun.Env, in *grokHookInput) {
	path := summaryPath(in)
	if path == "" || in.child() {
		return
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok || !(Agent{}).ContentConsented(env.Consent(r)) {
		return
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: name, semconv.TermaEvidenceSourceKey: sourceGrokSummary}
	hookrun.SpoolTitle(env, r, titleDir, in.SessionID, attrs, func() (string, bool, error) {
		return readTitle(path)
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
// named the session.
func readTitle(path string) (title string, found bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|hookrun.EvidenceOpenFlags, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, errors.New("summary.json is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, summaryMaxBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > summaryMaxBytes {
		return "", false, errors.New("summary.json is too long")
	}
	var summary struct {
		GeneratedTitle string `json:"generated_title"`
	}
	if err := json.Unmarshal(b, &summary); err != nil {
		return "", false, err
	}
	title = strings.TrimSpace(summary.GeneratedTitle)
	return title, title != "", nil
}
