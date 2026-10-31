package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/live"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// waits is a repository's hook environment that records the checks a Notification starts
// and the nudges they give, instead of starting a process and popping a notification.
type waits struct {
	root, stateDir string
	sp             *spool.Spool
	mu             sync.Mutex
	checks         [][2]string
	told           []string
}

func (w *waits) Notify(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.told = append(w.told, message)
}

func (w *waits) nudges() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.told...)
}

func newWaits(t *testing.T) *waits {
	t.Helper()
	w := &waits{root: newRepo(t), stateDir: t.TempDir()}
	w.sp, _ = spool.Open(t.TempDir())
	return w
}

// env runs a hook with stdin as its payload, notifications on unless off.
func (w *waits) env(stdin string, off bool) (hookrun.Env, *bytes.Buffer) {
	var out bytes.Buffer
	env := hookrun.Env{StateDir: w.stateDir, Cwd: w.root, Policy: hookruntest.Admitting(w.root), Team: hookruntest.Team,
		Stdin: strings.NewReader(stdin), Stdout: &out, Spool: w.sp, Version: "test",
		AwaitAnswer: func(path, id string) { w.checks = append(w.checks, [2]string{path, id}) }}
	if !off {
		env.Notifier = w
	}
	return env, &out
}

// run runs the Claude Code hook for event with the payload fields.
func (w *waits) run(t *testing.T, hook string, fields map[string]any) string {
	t.Helper()
	fields["session_id"], fields["cwd"] = "sess-wait", w.root
	payload, _ := json.Marshal(fields)
	env, out := w.env(string(payload), false)
	if err := (Agent{}).Events()[hook](context.Background(), env); err != nil {
		t.Fatalf("%s: %v", hook, err)
	}
	return out.String()
}

// transcriptLines is what a session's transcript holds as a wait begins.
const transcriptLines = `{"type":"assistant"}` + "\n"

// transcript is a session transcript of transcriptLines.
func (w *waits) transcript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sess-wait.jsonl")
	if err := os.WriteFile(path, []byte(transcriptLines), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// unlisted is a machine whose processes cannot be listed: its checks go by the transcript.
type unlisted struct{}

func (unlisted) Alive(int) bool             { return true }
func (unlisted) Children(int) ([]int, bool) { return nil, false }

func (w *waits) record(t *testing.T) (live.Wait, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.stateDir, claudeWaitingStateDir, hookrun.EvidenceID("sess-wait")+".json"))
	if err != nil {
		return live.Wait{}, false
	}
	var rec live.Wait
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec, true
}

// A permission prompt, or an MCP server's form or link, starts a wait and its check, with the
// transcript's size and Claude Code's process as they stand; the other notifications ask
// nothing of the developer.
func TestNotificationRecordsAWaitOnlyForWhatAsksTheDeveloper(t *testing.T) {
	for _, kind := range []string{"permission_prompt", "elicitation_dialog", "elicitation_url_dialog"} {
		w := newWaits(t)
		transcript := w.transcript(t)
		if out := w.run(t, "notification", map[string]any{"hook_event_name": "Notification", "notification_type": kind, "message": "Claude needs your permission",
			"transcript_path": transcript}); out != "" {
			t.Fatalf("%s printed %q", kind, out)
		}
		rec, ok := w.record(t)
		if !ok || rec.Agent != "Claude Code" || rec.Checkout != filepath.Base(w.root) || rec.ID == "" || len(w.checks) != 1 || w.checks[0][1] != rec.ID {
			t.Fatalf("%s: wait %+v (%v), checks %v", kind, rec, ok, w.checks)
		}
		if rec.Transcript != transcript || rec.TranscriptSize != int64(len(transcriptLines)) {
			t.Fatalf("%s: transcript %q at %d, want %q at %d", kind, rec.Transcript, rec.TranscriptSize, transcript, len(transcriptLines))
		}
		if _, listed := procinfo.Children(hookrun.HookCaller()); listed && hookrun.HookCaller() > 0 && rec.AgentPID != hookrun.HookCaller() {
			t.Fatalf("%s: watches process %d, want the hook's caller %d", kind, rec.AgentPID, hookrun.HookCaller())
		}
	}
	for _, kind := range []string{"idle_prompt", "auth_success", "agent_completed", ""} {
		w := newWaits(t)
		w.run(t, "notification", map[string]any{"hook_event_name": "Notification", "notification_type": kind})
		if _, ok := w.record(t); ok || len(w.checks) != 0 {
			t.Fatalf("%q started a wait: checks %v", kind, w.checks)
		}
	}
}

// With desktop notifications off, nobody would be told, so nothing waits.
func TestNotificationWithNotificationsOffRecordsNothing(t *testing.T) {
	w := newWaits(t)
	env, _ := w.env(`{"session_id":"sess-wait","notification_type":"permission_prompt"}`, true)
	if err := notification(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.record(t); ok || len(w.checks) != 0 {
		t.Fatalf("a wait with notifications off: checks %v", w.checks)
	}
}

// The developer's next prompt, any tool call ending, the turn's end and the session's end
// each end the wait; the prompt's hook still prints nothing.
func TestTheDevelopersNextMoveEndsTheWait(t *testing.T) {
	for _, end := range []struct {
		hook   string
		fields map[string]any
	}{
		{"user-prompt-submit", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "yes"}},
		{"post-tool-use", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion"}},
		{"post-tool-use", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash"}},
		{"stop", map[string]any{"hook_event_name": "Stop"}},
		{"session-end", map[string]any{"hook_event_name": "SessionEnd"}},
	} {
		w := newWaits(t)
		w.run(t, "notification", map[string]any{"hook_event_name": "Notification", "notification_type": "permission_prompt"})
		if _, ok := w.record(t); !ok {
			t.Fatal("no wait to end")
		}
		if out := w.run(t, end.hook, end.fields); out != "" {
			t.Fatalf("%s printed %q", end.hook, out)
		}
		if rec, ok := w.record(t); ok {
			t.Errorf("%s %v left the wait %+v", end.hook, end.fields["tool_name"], rec)
		}
	}
}

// A tool call that edits nothing only ends a wait: it records no files and spools nothing,
// now that every tool call runs the hook.
func TestPostToolUseOfAnotherToolRecordsNothing(t *testing.T) {
	w := newWaits(t)
	w.run(t, "post-tool-use", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./..."}})
	if evs := hookruntest.Spooled(t, w.sp); len(evs) != 0 {
		t.Fatalf("a Bash call spooled %+v", evs)
	}
	r, err := (hookrun.Env{StateDir: w.stateDir, Cwd: w.root, Policy: hookruntest.Admitting(w.root)}).Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ms, _ := r.Store.Manifests(); len(ms) != 0 {
		t.Fatalf("a Bash call recorded %+v", ms)
	}
	w.run(t, "post-tool-use", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Write", "tool_input": map[string]any{"file_path": filepath.Join(w.root, "a.go")}})
	if evs := hookruntest.Spooled(t, w.sp); len(evs) == 0 {
		t.Fatal("a Write call spooled nothing")
	}
}

// From the hook to the check: a wait still going on after two minutes nudges the developer
// once, naming the agent and the checkout; one the developer answered in time never does,
// through a hook or by a rejection only the transcript shows.
func TestWaitNudgesAfterTwoMinutesUnlessAnswered(t *testing.T) {
	for _, answer := range []string{"", "post-tool-use", "rejected"} {
		w := newWaits(t)
		transcript := w.transcript(t)
		synctest.Test(t, func(t *testing.T) {
			w.run(t, "notification", map[string]any{"hook_event_name": "Notification", "notification_type": "permission_prompt", "transcript_path": transcript})
			if len(w.checks) != 1 {
				t.Fatalf("checks %v", w.checks)
			}
			done := make(chan bool, 1)
			go func() { done <- live.AwaitAnswer(t.Context(), w.checks[0][0], w.checks[0][1], w, unlisted{}) }()
			time.Sleep(time.Minute)
			switch answer {
			case "post-tool-use":
				w.run(t, "post-tool-use", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion"})
			case "rejected":
				rejection := transcriptLines + `{"type":"user","message":{"content":"[Request interrupted by user for tool use]"}}` + "\n"
				if err := os.WriteFile(transcript, []byte(rejection), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(live.AnswerWait)
			nudged := <-done
			want := []string{"Claude Code is waiting for your answer · " + filepath.Base(w.root) + " · 2 min"}
			if answer != "" {
				want = nil
			}
			if got := w.nudges(); nudged != (answer == "") || strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("answer %q: nudged %v %q, want %q", answer, nudged, got, want)
			}
		})
	}
}
