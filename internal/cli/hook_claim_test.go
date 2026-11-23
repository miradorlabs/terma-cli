package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// collectedRepo is a git checkout the team policy collects, on a machine set up, and the
// test's working directory.
func collectedRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:acme/" + filepath.Base(root) + ".git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_HOOKS", "")
	admitHere(t, root, "team")
	signInHereAs(t, "u-hook")
	hookruntest.RelayOn(t, testApp.stateDir)
	t.Chdir(root)
	return root
}

// runHook runs `terma hook event` as an agent does, with payload on stdin.
func runHook(t *testing.T, event, payload string) {
	t.Helper()
	c := testApp.newHookCommand()
	c.SetArgs([]string{"--user", event})
	c.SetIn(strings.NewReader(payload))
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("terma hook %s: %v", event, err)
	}
}

// UserPromptSubmit sends nothing, yet still claims its session through `terma hook`: the
// claim reads the payload the handler consumed, and this is the claim at the start of every
// turn.
func TestCodexPromptHookStillClaims(t *testing.T) {
	root := collectedRepo(t)
	sid := "thread-prompt"
	runHook(t, "codex-user-prompt-submit", `{"session_id":"`+sid+`","turn_id":"turn-1","prompt":"hi","cwd":"`+root+`"}`)
	if got, ok := claim.Read(testApp.stateDir, sid, time.Now()); !ok || got.ProjectID != "team" {
		t.Errorf("the prompt hook claimed nothing: %+v %v", got, ok)
	}
}

// A Codex session running across teardown keeps calling the hooks it loaded at startup.
// Once torn down they keep and queue nothing, and a flush sends nothing.
func TestHooksSendNothingOnceTornDown(t *testing.T) {
	root := collectedRepo(t)
	payload := `{"session_id":"thread-across","turn_id":"turn-1","prompt":"hi","cwd":"` + root + `"}`
	runHook(t, "codex-session-start", payload)
	if _, err := os.Stat(filepath.Join(testApp.stateDir, spool.Dir)); err != nil {
		t.Fatalf("set up, the hook queued nothing: %v", err)
	}
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	for _, event := range []string{"codex-session-start", "codex-user-prompt-submit", "codex-post-tool-use", "codex-session-end"} {
		runHook(t, event, payload)
	}
	// A status line still renders, but captures and claims nothing.
	runHook(t, "statusline", `{"session_id":"thread-across","cwd":"`+root+`","rate_limits":{"five_hour":{"used_percentage":10}}}`)
	if out, err := runTerma(t, "spool", "flush"); err == nil || !strings.Contains(err.Error(), "not set up") {
		t.Errorf("a flush after teardown = %v:\n%s", err, out)
	}
	for _, dir := range append(testApp.hookStateDirs(), spool.Dir, claim.DirName) {
		if _, err := os.Stat(filepath.Join(testApp.stateDir, dir)); !os.IsNotExist(err) {
			t.Errorf("%s is back after teardown: %v", dir, err)
		}
	}
}

// An Antigravity hook under a person's sign-in queues what it collects in that person's
// lane, which the spool sends with their sign-in: no team key is involved.
func TestAntigravityHookQueuesInTheSignedInLane(t *testing.T) {
	root := collectedRepo(t)
	account := signInHereAs(t, "u-agy")
	runHook(t, "antigravity-pre-invocation", `{"conversationId":"agy-lane-1","cwd":"`+hookruntest.InJSON(root)+`","modelName":"gemini-3.8-flash-low","initialNumSteps":1,"invocationNum":0}`)
	events, err := spoolForTest(t).Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, e := range events {
		if e.Account != account || e.NoPerson {
			t.Errorf("%s queued outside the signed-in lane: account %q, no person %v", e.Name, e.Account, e.NoPerson)
		}
		if e.Name == "terma.session.start" && e.SessionID == "agy-lane-1" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("queued %d session starts for the conversation, want 1 (%d events)", starts, len(events))
	}
}

// What Antigravity's transcript and status line give, a model call and the plan's quota
// and account, queues in the signed-in person's lane too.
func TestAntigravityUsageAndSeatQueueInTheSignedInLane(t *testing.T) {
	root := collectedRepo(t)
	account := signInHereAs(t, "u-agy-usage")
	const id = "agy-lane-usage"
	transcript := filepath.Join(t.TempDir(), "brain", id, ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T16:50:00Z","content":"x"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:50:01Z","model":"gemini-3.8-flash-low","input_tokens":100,"cache_read_tokens":20,"output_tokens":5}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := `"conversationId":"` + id + `","cwd":"` + hookruntest.InJSON(root) + `","transcriptPath":"` + hookruntest.InJSON(transcript) + `","modelName":"gemini-3.8-flash-low"`
	runHook(t, "antigravity-pre-invocation", `{`+payload+`,"initialNumSteps":1,"invocationNum":0}`)
	runHook(t, "antigravity-post-invocation", `{`+payload+`,"initialNumSteps":1,"invocationNum":0}`)
	runHook(t, "antigravity-statusline", `{"conversation_id":"`+id+`","session_id":"`+id+`","cwd":"`+hookruntest.InJSON(root)+`","plan_tier":"Google AI Ultra","email":"dev@example.com",`+
		`"quota":{"gemini-5h":{"remaining_fraction":0.9,"reset_time":"2026-11-23T20:00:00Z"}}}`)
	events, err := spoolForTest(t).Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range events {
		if e.SessionID != id {
			continue
		}
		seen[e.Name]++
		if e.Account != account || e.NoPerson {
			t.Errorf("%s queued outside the signed-in lane: account %q, no person %v", e.Name, e.Account, e.NoPerson)
		}
	}
	for _, name := range []string{"terma.model_call", "terma.session.quota", "terma.session.account"} {
		if seen[name] != 1 {
			t.Errorf("queued %d %s for the conversation, want 1: %v", seen[name], name, seen)
		}
	}
}

// With hooks off, agy's hooks still reach terma: each ends the conversation's turn, so a
// turn begun meanwhile sends nothing as the one before's once hooks are back.
func TestAntigravityTurnEndsWhileHooksAreOff(t *testing.T) {
	root := collectedRepo(t)
	const id = "agy-hooks-off"
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", id)
	transcript := filepath.Join(t.TempDir(), "brain", id, ".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := func(steps ...string) {
		var b strings.Builder
		for _, step := range steps {
			b.WriteString(`{"step_index":` + step + `,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:50:01Z","model":"gemini-3.8-flash-low","input_tokens":100,"output_tokens":5}` + "\n")
		}
		if err := os.WriteFile(transcript, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	payload := `{"conversationId":"` + id + `","cwd":"` + hookruntest.InJSON(root) + `","transcriptPath":"` + hookruntest.InJSON(transcript) + `",`
	runHook(t, "antigravity-pre-invocation", payload+`"initialNumSteps":1,"invocationNum":0}`)
	calls("1")
	runHook(t, "antigravity-post-invocation", payload+`"initialNumSteps":1,"invocationNum":0}`)
	t.Setenv("TERMA_HOOKS", "0")
	runHook(t, "antigravity-stop", payload+`"executionNum":0}`)
	runHook(t, "antigravity-pre-invocation", payload+`"initialNumSteps":2,"invocationNum":0}`)
	t.Setenv("TERMA_HOOKS", "")
	calls("1", "3")
	runHook(t, "antigravity-post-invocation", payload+`"initialNumSteps":2,"invocationNum":0}`)
	events, err := spoolForTest(t).Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, e := range events {
		if e.Name == semconv.TermaModelCallEvent && e.SessionID == id {
			sent = append(sent, fmt.Sprint(e.Attrs[semconv.TermaModelCallIDKey], " ", e.Attrs[semconv.TermaTurnIDKey]))
		}
	}
	if got := strings.Join(sent, ","); got != "step-1 turn-1" {
		t.Fatalf("sent %q, want turn 1's call alone", got)
	}
}
