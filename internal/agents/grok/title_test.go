package grok_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// captureSessions is the group folder of the captured transcript paths: each payload names
// ~/.grok/sessions/<encoded cwd>/<session>/updates.jsonl.
var captureSessions = regexp.MustCompile(`/home/dev/\.grok/sessions/[^/"]+/`)

// titled replays fixture with every transcript path moved under a sessions folder of the test's
// own, where summaries[<session>] is written as that session's summary.json.
func titled(t *testing.T, fixture string, summaries map[string]string, o replayOpts) *replayRun {
	t.Helper()
	dir := t.TempDir()
	for session, body := range summaries {
		hookruntest.WriteFile(t, filepath.Join(dir, "g", session), "summary.json", body)
	}
	edit := o.edit
	o.edit = func(event string, payload []byte) []byte {
		payload = captureSessions.ReplaceAll(payload, []byte(hookruntest.InJSON(filepath.Join(dir, "g"))+"/"))
		if edit != nil {
			payload = edit(event, payload)
		}
		return payload
	}
	return replay(t, fixture, o)
}

// titles are the names spooled, as "<session> <title>".
func (r *replayRun) titles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range hookruntest.Named(hookruntest.Spooled(t, r.sp), semconv.TermaSessionTitleEvent) {
		if e.Attrs[semconv.GenAIMainAgentNameKey] != "grok" || e.Attrs[semconv.TermaEvidenceSourceKey] != "grok_summary" {
			t.Errorf("title attributes %v", e.Attrs)
		}
		title, _ := e.Attrs[semconv.TermaSessionTitleKey].(string)
		out = append(out, e.SessionID+" "+title)
	}
	return out
}

const forkSession = "01a20a05-009b-7230-b3e5-fa045ca9d84d"

// The name grok gave a session goes once, from its first Stop, however many Stops and the
// SessionEnd read it again; a summary grok has not titled yet, or one terma cannot read, sends
// nothing.
func TestTitleGoesOnceFromTheSummary(t *testing.T) {
	run := titled(t, "headless_fork_2_fork_session", map[string]string{
		forkSession: `{"info":{"id":"` + forkSession + `"},"session_summary":"x","generated_title":"  Fix the login bug  ","num_messages":3}`,
	}, replayOpts{})
	if got := run.titles(t); strings.Join(got, ",") != forkSession+" Fix the login bug" {
		t.Errorf("titles %q", got)
	}
	for name, body := range map[string]string{
		"untitled":  `{"info":{"id":"` + forkSession + `"},"num_messages":1}`,
		"blank":     `{"generated_title":"   "}`,
		"malformed": `{"generated_title":`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := titled(t, "headless_fork_2_fork_session", map[string]string{forkSession: body}, replayOpts{}).titles(t); len(got) != 0 {
				t.Errorf("titles %q", got)
			}
		})
	}
	t.Run("no summary", func(t *testing.T) {
		if got := titled(t, "headless_fork_2_fork_session", nil, replayOpts{}).titles(t); len(got) != 0 {
			t.Errorf("titles %q", got)
		}
	})
}

// A rename is a new name: it goes at the next Stop.
func TestTitleRenameGoesAgain(t *testing.T) {
	dir := t.TempDir()
	summary := filepath.Join(dir, "g", forkSession, "summary.json")
	hookruntest.WriteFile(t, filepath.Dir(summary), "summary.json", `{"generated_title":"Fix the login bug"}`)
	stops := 0
	run := replay(t, "headless_fork_2_fork_session", replayOpts{edit: func(event string, payload []byte) []byte {
		if event == "stop" {
			if stops++; stops == 2 {
				if err := os.WriteFile(summary, []byte(`{"generated_title":"Login fix","title_is_manual":true}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return captureSessions.ReplaceAll(payload, []byte(hookruntest.InJSON(filepath.Join(dir, "g"))+"/"))
	}})
	if got := run.titles(t); strings.Join(got, ",") != forkSession+" Fix the login bug,"+forkSession+" Login fix" {
		t.Errorf("titles %q", got)
	}
}

// A subagent's session is not titled; its parent is.
func TestTitleNamesNoSubagent(t *testing.T) {
	run := titled(t, "headless_subagent_worktree", map[string]string{
		worktreeParent: `{"generated_title":"Parent task"}`,
		worktreeChild:  `{"generated_title":"Child task"}`,
	}, replayOpts{})
	if got := run.titles(t); strings.Join(got, ",") != worktreeParent+" Parent task" {
		t.Errorf("titles %q", got)
	}
}

// The name restates the first prompt: with grok not among the developer's agents it is not
// spooled. The policy's prompt switch is delivery's (internal/delivery).
func TestTitleNeedsConsent(t *testing.T) {
	run := titled(t, "headless_fork_2_fork_session", map[string]string{forkSession: `{"generated_title":"Fix the login bug"}`},
		replayOpts{agents: []string{"claude"}})
	if got := run.titles(t); len(got) != 0 {
		t.Errorf("titles %q", got)
	}
}

// Only the session's own updates.jsonl leads to a summary: a transcript path of another shape,
// or another session's, names none.
func TestTitleReadsOnlyTheSessionsOwnSummary(t *testing.T) {
	for name, path := range map[string]string{
		"another session": "g/01a20a05-ffff-7230-b3e5-fa045ca9d84d/updates.jsonl",
		"another file":    "g/" + forkSession + "/chat_history.jsonl",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			hookruntest.WriteFile(t, filepath.Join(dir, "g", forkSession), "summary.json", `{"generated_title":"Fix the login bug"}`)
			hookruntest.WriteFile(t, filepath.Join(dir, "g", "01a20a05-ffff-7230-b3e5-fa045ca9d84d"), "summary.json", `{"generated_title":"Another"}`)
			wrong := regexp.MustCompile(`"transcript_path":"[^"]*"`)
			run := replay(t, "headless_fork_2_fork_session", replayOpts{edit: func(_ string, payload []byte) []byte {
				return wrong.ReplaceAll(payload, []byte(`"transcript_path":"`+hookruntest.InJSON(filepath.Join(dir, path))+`"`))
			}})
			if got := run.titles(t); len(got) != 0 {
				t.Errorf("titles %q", got)
			}
		})
	}
}
