package antigravity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The fixtures are real agy 1.5.13 transcripts with content scrubbed:
// transcript_turns.jsonl is three completed turns and a fourth cancelled after its first
// call (steps 17–20, no hook ran); transcript_subagent_*.jsonl a conversation that opened
// one subagent, and the subagent's own.

const (
	turnsConversation  = "81356ffa-7411-49ac-9916-142dea1662dc"
	parentConversation = "bbdd1b9a-7667-4fd7-803e-a323c4b7a90d"
	childConversation  = "2c917b91-90c3-4c4a-bab2-e861d1cf48ed"
)

// transcriptRig is a repository and an agy brain folder whose transcript grows as a test
// appends a fixture's lines.
type transcriptRig struct {
	t          *testing.T
	root       string
	sp         *spool.Spool
	stateDir   string
	transcript string
	lines      []string
	id         string
}

func newTranscriptRig(t *testing.T, id, fixture string) *transcriptRig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(t.TempDir())
	brain := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli", "brain")
	transcript := filepath.Join(brain, id, ".system_generated", "logs", antigravityTranscriptName)
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	return &transcriptRig{t: t, root: hookruntest.InitRepo(t), sp: sp, stateDir: t.TempDir(), transcript: transcript,
		lines: strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n"), id: id}
}

// write makes the transcript the fixture's lines [0, n), each ending in a newline.
func (g *transcriptRig) write(n int) {
	g.t.Helper()
	var b strings.Builder
	for _, l := range g.lines[:n] {
		b.WriteString(strings.TrimSuffix(l, "\n") + "\n")
	}
	if err := os.WriteFile(g.transcript, []byte(b.String()), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

func (g *transcriptRig) hook(h func(context.Context, hookrun.Env) error, extra string) {
	g.t.Helper()
	payload := `{"conversationId":"` + g.id + `","modelName":"gemini-3.8-flash-low","cwd":"` + g.root + `","transcriptPath":"` + g.transcript + `",` + extra + `}`
	var out bytes.Buffer
	env := hookrun.Env{StateDir: g.stateDir, Now: time.Now(), Cwd: g.root, Policy: hookruntest.Admitting(g.root), Stdin: strings.NewReader(payload), Stdout: &out, Spool: g.sp, Version: "test"}
	if err := h(context.Background(), env); err != nil {
		g.t.Fatal(err)
	}
}

// drain is what the hooks spooled since the last drain, by event name.
func (g *transcriptRig) drain() map[string][]spool.Event {
	out := map[string][]spool.Event{}
	for _, e := range hookruntest.Spooled(g.t, g.sp) {
		out[e.Name] = append(out[e.Name], e)
	}
	return out
}

func callSteps(events []spool.Event) []string {
	var steps []string
	for _, e := range events {
		steps = append(steps, e.Attrs[semconv.TermaModelCallIDKey].(string))
	}
	return steps
}

func TestAntigravityCallsGoOnceEachWithTheirTokens(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")

	// Turn one: three calls, the last on disk before PostInvocation runs.
	g.write(1)
	g.hook(preInvocation, `"initialNumSteps":1,"invocationNum":0`)
	if got := g.drain()[semconv.TermaModelCallEvent]; len(got) != 0 {
		t.Fatalf("calls before any was made: %v", got)
	}
	g.write(4)
	g.hook(postToolUse, `"stepIdx":2,"toolCall":{"name":"write_file","args":{}}`)
	g.write(7)
	g.hook(postInvocation, `"initialNumSteps":5,"invocationNum":2`)
	g.hook(stop, `"executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	got := g.drain()
	calls := got[semconv.TermaModelCallEvent]
	if s := strings.Join(callSteps(calls), ","); s != "step-1,step-3,step-6" {
		t.Fatalf("turn one sent %s", s)
	}
	// Step 3: input 2164 uncached + 6806 read from the cache, 50 out.
	c := calls[1]
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaEvidenceSourceKey: sourceAntigravityTranscript,
		semconv.GenAIRequestModelKey: "gemini-3.8-flash-low", semconv.TermaTurnIDKey: "turn-1", semconv.TermaStepIndexKey: int64(3),
		semconv.GenAIUsageInputTokensKey: int64(2164 + 6806), semconv.GenAIUsageCacheReadInputTokensKey: int64(6806),
		semconv.GenAIUsageOutputTokensKey: int64(50), semconv.TermaUsageStatusKey: semconv.TermaUsageStatusPartial,
		semconv.TermaOperationStatusKey: "completed",
	} {
		if v := norm(c.Attrs[k]); v != norm(want) {
			t.Errorf("call[%q] = %v (%T), want %v", k, c.Attrs[k], c.Attrs[k], want)
		}
	}
	// Thinking is inside output, never a zero of its own.
	if _, ok := c.Attrs[semconv.GenAIUsageReasoningOutputTokensKey]; ok {
		t.Errorf("a reasoning count agy never gave: %v", c.Attrs)
	}
	if want := time.Date(2026, 11, 20, 16, 51, 50, 0, time.UTC); !c.Time.Equal(want) {
		t.Errorf("call time %v", c.Time)
	}
	// The hooks that read the transcript make no usage claim of their own.
	for _, o := range got[semconv.TermaSessionObservationEvent] {
		if _, ok := o.Attrs[semconv.TermaUsageStatusKey]; ok {
			t.Errorf("%s observation still says usage %v", o.Attrs[semconv.TermaHookEventKey], o.Attrs[semconv.TermaUsageStatusKey])
		}
	}

	// Turns two and three, each read at its Stop; a second Stop sends nothing again.
	g.write(12)
	g.hook(preInvocation, `"initialNumSteps":8,"invocationNum":0`)
	g.hook(stop, `"executionNum":1,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	g.hook(stop, `"executionNum":1,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	calls = g.drain()[semconv.TermaModelCallEvent]
	if s := strings.Join(callSteps(calls), ","); s != "step-9,step-11" {
		t.Fatalf("turn two sent %s", s)
	}
	if calls[0].Attrs[semconv.TermaTurnIDKey] != "turn-8" {
		t.Errorf("turn two's call in %v", calls[0].Attrs[semconv.TermaTurnIDKey])
	}
	g.write(17)
	g.hook(stop, `"executionNum":2,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	if s := strings.Join(callSteps(g.drain()[semconv.TermaModelCallEvent]), ","); s != "step-14,step-16" {
		t.Fatalf("turn three sent %s", s)
	}

	// Turn four is cancelled after its first call: no hook runs. Its call goes with the
	// next turn's first hook, in its own turn, and once.
	g.write(21)
	g.lines = append(g.lines, `{"step_index":21,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T16:55:00Z","content":"<scrubbed>"}`)
	g.write(22)
	g.hook(preInvocation, `"initialNumSteps":22,"invocationNum":0`)
	g.hook(postInvocation, `"initialNumSteps":22,"invocationNum":0`)
	calls = g.drain()[semconv.TermaModelCallEvent]
	if s := strings.Join(callSteps(calls), ","); s != "step-19" {
		t.Fatalf("the cancelled turn sent %s", s)
	}
	if calls[0].Attrs[semconv.TermaTurnIDKey] != "turn-18" || norm(calls[0].Attrs[semconv.GenAIUsageInputTokensKey]) != norm(int64(10330)) {
		t.Errorf("cancelled call: %v", calls[0].Attrs)
	}
}

// Compaction rewrites the transcript in place: what was sent before stays sent.
func TestAntigravityRewrittenTranscriptSendsNoCallTwice(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(12)
	g.hook(stop, `"executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	if s := strings.Join(callSteps(g.drain()[semconv.TermaModelCallEvent]), ","); s != "step-1,step-3,step-6,step-9,step-11" {
		t.Fatalf("first read sent %s", s)
	}
	// A rewrite as long as the file was, with the old steps and a new call after them.
	rewritten := append([]string{}, g.lines[7:12]...)
	rewritten = append(rewritten, g.lines[12:17]...)
	g.lines = rewritten
	g.write(len(rewritten))
	g.hook(stop, `"executionNum":1,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	if s := strings.Join(callSteps(g.drain()[semconv.TermaModelCallEvent]), ","); s != "step-14,step-16" {
		t.Fatalf("after the rewrite, sent %s", s)
	}
}

// A conversation that opens a subagent names the child's conversation in the result of
// invoke_subagent; the parent's next hook links the two.
func TestAntigravitySubagentIsLinkedToItsParent(t *testing.T) {
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	g.write(3)
	g.hook(postInvocation, `"initialNumSteps":1,"invocationNum":0`)
	got := g.drain()
	var link *spool.Event
	for _, e := range got[semconv.TermaSessionStartEvent] {
		if e.SessionID == childConversation {
			link = &e
		}
	}
	if link == nil || link.Attrs[semconv.TermaSessionParentIDKey] != parentConversation || link.Attrs[semconv.GenAIMainAgentNameKey] != antigravityTool {
		t.Fatalf("no link from the child to its parent: %v", got[semconv.TermaSessionStartEvent])
	}
	for k, v := range link.Attrs {
		if s, ok := v.(string); ok && strings.Contains(s, "/") {
			t.Errorf("link carries a path in %s: %v", k, v)
		}
	}
	g.write(len(g.lines))
	g.hook(stop, `"executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	got = g.drain()
	if len(got[semconv.TermaSessionStartEvent]) != 0 {
		t.Errorf("the link went twice: %v", got[semconv.TermaSessionStartEvent])
	}
	if s := strings.Join(callSteps(got[semconv.TermaModelCallEvent]), ","); s != "step-3,step-5" {
		t.Errorf("parent's later calls: %s", s)
	}

	// The child's own hooks send its own calls under its own conversation.
	c := newTranscriptRig(t, childConversation, "transcript_subagent_child.jsonl")
	c.write(len(c.lines))
	c.hook(stop, `"executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	calls := c.drain()[semconv.TermaModelCallEvent]
	if len(calls) != 4 {
		t.Fatalf("child sent %d calls, want 4", len(calls))
	}
	for _, e := range calls {
		if e.SessionID != childConversation {
			t.Errorf("child's call under %s", e.SessionID)
		}
	}
}

// A path that is not this conversation's transcript in agy's brain folder is never read,
// and a hook that could not read one says usage was unavailable.
func TestAntigravityReadsOnlyTheConversationsTranscript(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(7)
	other := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(g.transcript)))), parentConversation, ".system_generated", "logs", antigravityTranscriptName)
	for _, p := range []string{other, filepath.Join(t.TempDir(), antigravityTranscriptName), "relative/transcript_full.jsonl"} {
		if got := antigravityTranscript(&antigravityHookInput{ConversationID: turnsConversation, TranscriptPath: p}); got != "" {
			t.Errorf("%s read as the transcript", p)
		}
	}
	// The compact copy's path names the full one beside it.
	compact := filepath.Join(filepath.Dir(g.transcript), "transcript.jsonl")
	if got := antigravityTranscript(&antigravityHookInput{ConversationID: turnsConversation, TranscriptPath: compact}); got != g.transcript {
		t.Errorf("compact path read as %q", got)
	}

	g.transcript = other
	g.hook(stop, `"executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	got := g.drain()
	if len(got[semconv.TermaModelCallEvent]) != 0 {
		t.Fatalf("calls from another conversation's transcript: %v", got[semconv.TermaModelCallEvent])
	}
	for _, o := range got[semconv.TermaSessionObservationEvent] {
		if o.Attrs[semconv.TermaUsageStatusKey] != hookrun.StatusUnavailable {
			t.Errorf("observation usage = %v, want unavailable", o.Attrs[semconv.TermaUsageStatusKey])
		}
	}
}

func TestAntigravityCallCounts(t *testing.T) {
	step := int64(4)
	for _, tc := range []struct {
		name, in, cache, out string
		status               string
		input                any
	}{
		{"all", "100", "50", "7", semconv.TermaUsageStatusPartial, int64(150)},
		{"no cache read", "100", "", "7", semconv.TermaUsageStatusPartial, int64(100)},
		{"none", "", "", "", semconv.TermaUsageStatusUnavailable, nil},
		{"negative", "-1", "0", "7", semconv.TermaUsageStatusInvalid, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := antigravityStep{StepIndex: &step, Type: "PLANNER_RESPONSE", Status: "DONE", InputTokens: []byte(tc.in), CacheReadTokens: []byte(tc.cache), OutputTokens: []byte(tc.out)}
			a := antigravityCallEvent(turnsConversation, s, "turn-1").Attrs
			if a[semconv.TermaUsageStatusKey] != tc.status || a[semconv.GenAIUsageInputTokensKey] != tc.input {
				t.Errorf("attrs %v", a)
			}
		})
	}
}

// norm is v as the spool hands it back: every number a float64.
func norm(v any) any {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return v
}

// A conversation's calls made where the team's policy collects nothing never leave, even
// once it goes on in a repository that is collected.
func TestAntigravityCallsOutsideCollectionNeverLeave(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(7)
	excluded := hookruntest.InitRepo(t)
	payload := `{"conversationId":"` + g.id + `","cwd":"` + excluded + `","transcriptPath":"` + g.transcript + `","executionNum":0,"fullyIdle":true}`
	env := hookrun.Env{StateDir: g.stateDir, Now: time.Now(), Cwd: excluded, Policy: hookruntest.Admitting(g.root), Stdin: strings.NewReader(payload), Stdout: &bytes.Buffer{}, Spool: g.sp, Version: "test"}
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := g.drain(); len(got) != 0 {
		t.Fatalf("an excluded repository's hook spooled %v", got)
	}
	g.write(12)
	g.hook(stop, `"executionNum":1,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	if s := strings.Join(callSteps(g.drain()[semconv.TermaModelCallEvent]), ","); s != "step-9,step-11" {
		t.Fatalf("in the collected repository, sent %s", s)
	}
}

// Tool output that quotes invoke_subagent's result links nothing.
func TestAntigravityQuotedSubagentResultLinksNothing(t *testing.T) {
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	// The fixture's subagent result, as the output of a command that printed it.
	quoted := g.lines[2]
	g.lines = []string{g.lines[0],
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T17:40:11Z","model":"gemini-3.8-flash-high","input_tokens":10,"cache_read_tokens":0,"output_tokens":1,"tool_calls":[{"name":"run_command","args":{}}]}`,
		quoted}
	g.write(3)
	g.hook(stop, `"executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)
	if starts := g.drain()[semconv.TermaSessionStartEvent]; len(starts) != 0 {
		t.Fatalf("a quoted result linked a subagent: %v", starts)
	}
}
