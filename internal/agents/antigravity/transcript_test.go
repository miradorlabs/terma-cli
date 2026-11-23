package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The fixtures are real agy 1.5.13 transcripts with content scrubbed, which the rig writes
// back as made-up text in agy's shapes (said):
// transcript_turns.jsonl is three completed turns and a fourth cancelled after its first
// call (steps 17–20, no hook ran); transcript_subagent_*.jsonl a conversation that opened
// one subagent, and the subagent's own. Line i of transcript_turns.jsonl is step i.

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
	spDir      string
	stateDir   string
	transcript string
	lines      []string
	id         string
	// admitted are the checkouts the policy collects; root at first.
	admitted []string
	// agents are the agents the developer chose: Antigravity at first, so content may go.
	agents []string
	// noPrompts and noToolContent switch the policy's content off.
	noPrompts, noToolContent bool
	// workspaces, when set, are every payload's workspacePaths.
	workspaces []string
	// seen is every event drain returned.
	seen []spool.Event
}

func newTranscriptRig(t *testing.T, id, fixture string) *transcriptRig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	spDir := t.TempDir()
	sp, _ := hookruntest.OpenSpool(spDir)
	brain := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli", "brain")
	transcript := filepath.Join(brain, id, ".system_generated", "logs", antigravityTranscriptName)
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	root := hookruntest.InitRepo(t)
	return &transcriptRig{t: t, root: root, sp: sp, spDir: spDir, stateDir: t.TempDir(), transcript: transcript,
		lines: strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n"), id: id, admitted: []string{root},
		agents: []string{Agent{}.Name()}}
}

// said is the made-up content the rig writes for a scrubbed line: a prompt in agy's
// envelope, a reply, a tool's result or a notice, each naming its step.
func said(kind string, step int64) string {
	n := strconv.FormatInt(step, 10)
	switch kind {
	case "USER_INPUT":
		return "<USER_REQUEST>\nmade-up prompt " + n + "\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nmade-up metadata " + n + "\n</ADDITIONAL_METADATA>"
	case "PLANNER_RESPONSE":
		return "made-up reply " + n
	case "GENERIC":
		return "Created At: 2026-11-20T16:51:50Z\nmade-up result " + n
	}
	return "made-up notice " + n
}

// fill writes a scrubbed line's content back as said's.
func fill(line string) string {
	if !strings.Contains(line, `"content":"<scrubbed>"`) {
		return line
	}
	var s struct {
		Step int64  `json:"step_index"`
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &s) != nil {
		return line
	}
	text, _ := json.Marshal(said(s.Type, s.Step))
	return strings.Replace(line, `"content":"<scrubbed>"`, `"content":`+string(text), 1)
}

// spoke reports whether any event drain returned carries the made-up content of step, of
// any kind, in any attribute.
func (g *transcriptRig) spoke(step int64) bool {
	n := " " + strconv.FormatInt(step, 10)
	for _, e := range g.seen {
		for _, v := range e.Attrs {
			b, _ := json.Marshal(v)
			for _, kind := range []string{"prompt", "reply", "result"} {
				if strings.Contains(string(b), "made-up "+kind+n+`"`) || strings.Contains(string(b), "made-up "+kind+n+`\n`) {
					return true
				}
			}
		}
	}
	return false
}

// write makes the transcript the fixture's lines [0, n), each ending in a newline.
func (g *transcriptRig) write(n int) {
	g.t.Helper()
	var b strings.Builder
	for _, l := range g.lines[:n] {
		b.WriteString(fill(strings.TrimSuffix(l, "\n")) + "\n")
	}
	if err := os.WriteFile(g.transcript, []byte(b.String()), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// add appends lines to the fixture and writes the transcript whole.
func (g *transcriptRig) add(lines ...string) {
	g.t.Helper()
	g.lines = append(g.lines, lines...)
	g.write(len(g.lines))
}

func (g *transcriptRig) policy() config.Policy {
	p := config.DefaultPolicy()
	p.Repositories = nil
	for _, root := range g.admitted {
		p.Repositories = append(p.Repositories, hookruntest.Admitting(root).Repositories...)
	}
	p.IncludePrompts, p.IncludeToolContent = !g.noPrompts, !g.noToolContent
	return p
}

func (g *transcriptRig) env(cwd, extra string) hookrun.Env {
	payload := `{"conversationId":"` + g.id + `","modelName":"gemini-3.8-flash-low","cwd":"` + cwd + `","transcriptPath":"` + g.transcript + `",` + extra + `}`
	if len(g.workspaces) > 0 {
		paths, _ := json.Marshal(g.workspaces)
		payload = strings.TrimSuffix(payload, "}") + `,"workspacePaths":` + string(paths) + `}`
	}
	return hookrun.Env{StateDir: g.stateDir, Now: time.Now(), Cwd: cwd, Policy: g.policy(), Agents: g.agents, Stdin: strings.NewReader(payload), Stdout: &bytes.Buffer{}, Spool: g.sp, Version: "test"}
}

// hookAt runs h as a hook whose agent works in cwd.
func (g *transcriptRig) hookAt(h func(context.Context, hookrun.Env) error, cwd, extra string) {
	g.t.Helper()
	if err := h(context.Background(), g.env(cwd, extra)); err != nil {
		g.t.Fatal(err)
	}
}

func (g *transcriptRig) hook(h func(context.Context, hookrun.Env) error, extra string) {
	g.t.Helper()
	g.hookAt(h, g.root, extra)
}

// drain is what the hooks spooled since the last drain, by event name.
func (g *transcriptRig) drain() map[string][]spool.Event {
	out := map[string][]spool.Event{}
	for _, e := range hookruntest.Spooled(g.t, g.sp) {
		out[e.Name] = append(out[e.Name], e)
		g.seen = append(g.seen, e)
	}
	return out
}

// calls is the model calls spooled since the last drain, as "step-N turn".
func (g *transcriptRig) calls() string {
	var out []string
	for _, e := range g.drain()[semconv.TermaModelCallEvent] {
		turn, _ := e.Attrs[semconv.TermaTurnIDKey].(string)
		out = append(out, e.Attrs[semconv.TermaModelCallIDKey].(string)+" "+turn)
	}
	return strings.Join(out, ",")
}

// lost is the calls the rig's spool counted lost as turn_ended.
func (g *transcriptRig) lost() int {
	l, err := spool.ReadLosses(g.spDir, time.Now())
	if err != nil {
		g.t.Fatal(err)
	}
	return l.Counts[sendas.LostTurnEnded]
}

// Hook payloads.
const (
	stopped       = `"executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`
	invoked       = `"initialNumSteps":5,"invocationNum":2`
	userInput21   = `{"step_index":21,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T16:55:00Z","content":"<scrubbed>"}`
	call22        = `{"step_index":22,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:55:01Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1}`
	userInput23   = `{"step_index":23,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T16:56:00Z","content":"<scrubbed>"}`
	call24        = `{"step_index":24,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:56:01Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1}`
	subagentCall1 = `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T17:40:11Z","model":"gemini-3.8-flash-high","input_tokens":10,"cache_read_tokens":0,"output_tokens":1,"tool_calls":[{"name":"invoke_subagent","args":{}},{"name":"run_command","args":{}}]}`
)

func begin(n string) string { return `"initialNumSteps":` + n + `,"invocationNum":0` }

func TestAntigravityCallsGoOnceEachWithTheirTokens(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")

	// Turn one: three calls, the last on disk before PostInvocation runs.
	g.write(1)
	g.hook(preInvocation, begin("1"))
	if got := g.drain()[semconv.TermaModelCallEvent]; len(got) != 0 {
		t.Fatalf("calls before any was made: %v", got)
	}
	g.write(4)
	g.hook(postToolUse, `"stepIdx":2,"toolCall":{"name":"write_file","args":{}}`)
	g.write(7)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
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
	// Stamped from the conversation's clock: when agy wrote the line, raised past every event
	// stamped before it (here the hooks', which ran at the real time).
	if want := time.Date(2026, 11, 20, 16, 51, 50, 0, time.UTC); c.Time.Before(want) || !c.Time.After(calls[0].Time) {
		t.Errorf("call time %v, the call before %v", c.Time, calls[0].Time)
	}
	// The hooks that read the transcript make no usage claim of their own.
	for _, o := range got[semconv.TermaSessionObservationEvent] {
		if _, ok := o.Attrs[semconv.TermaUsageStatusKey]; ok {
			t.Errorf("%s observation still says usage %v", o.Attrs[semconv.TermaHookEventKey], o.Attrs[semconv.TermaUsageStatusKey])
		}
	}

	// Turns two and three, each read at its Stop; a second Stop sends nothing again.
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(stop, stopped)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("turn two sent %s", s)
	}
	g.write(13)
	g.hook(preInvocation, begin("13"))
	g.write(17)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-14 turn-13,step-16 turn-13" {
		t.Fatalf("turn three sent %s", s)
	}
	if n := g.lost(); n != 0 {
		t.Fatalf("%d lost before any turn was cancelled", n)
	}

	// Turn four is cancelled after its first call: no hook of it runs after the call. The
	// next turn's hooks never send it as their own, and count it lost, once.
	g.write(18)
	g.hook(preInvocation, begin("18"))
	g.write(21)
	g.add(userInput21)
	g.hook(preInvocation, begin("22"))
	g.hook(postInvocation, begin("22"))
	if s := g.calls(); s != "" {
		t.Fatalf("the cancelled turn's call went: %s", s)
	}
	if n := g.lost(); n != 1 {
		t.Fatalf("counted %d lost, want the cancelled call once", n)
	}
	// The new turn's own call goes.
	g.add(call22)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-22 turn-22" || g.lost() != 1 {
		t.Fatalf("turn 22 sent %q, lost %d", s, g.lost())
	}
	// The cancelled turn's reply was passed with its call, and never leaves.
	if g.spoke(19) {
		t.Fatal("the cancelled turn's reply left")
	}
}

func callSteps(events []spool.Event) []string {
	var steps []string
	for _, e := range events {
		steps = append(steps, e.Attrs[semconv.TermaModelCallIDKey].(string))
	}
	return steps
}

// Compaction rewrites the transcript in place: what was sent before stays sent.
func TestAntigravityRewrittenTranscriptSendsNoCallTwice(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(7)
	g.hook(stop, stopped)
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(stop, stopped)
	if s := strings.Join(callSteps(g.drain()[semconv.TermaModelCallEvent]), ","); s != "step-1,step-3,step-6,step-9,step-11" {
		t.Fatalf("first read sent %s", s)
	}
	// A rewrite that keeps turn 8's steps, then turn 13 after them.
	g.lines = append([]string{}, g.lines[7:17]...)
	g.write(6)
	g.hook(preInvocation, begin("13"))
	g.write(len(g.lines))
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-14 turn-13,step-16 turn-13" {
		t.Fatalf("after the rewrite, sent %s", s)
	}
}

// A conversation that opens a subagent names the child's conversation in the result of
// invoke_subagent; the parent's next hook links the two. The child runs its own turn,
// turn-0, recorded before its transcript exists.
func TestAntigravitySubagentIsLinkedToItsParent(t *testing.T) {
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(3)
	g.hook(postInvocation, invoked)
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
	g.hook(stop, stopped)
	got = g.drain()
	if len(got[semconv.TermaSessionStartEvent]) != 0 {
		t.Errorf("the link went twice: %v", got[semconv.TermaSessionStartEvent])
	}
	if s := strings.Join(callSteps(got[semconv.TermaModelCallEvent]), ","); s != "step-3,step-5" {
		t.Errorf("parent's later calls: %s", s)
	}

	// The child's first hook runs before its transcript exists, as agy runs it.
	c := newTranscriptRig(t, childConversation, "transcript_subagent_child.jsonl")
	c.hook(preInvocation, begin("0"))
	// Saved with nothing read: the next hook must not begin again past the child's calls.
	if s := c.readCallState(); s.Before != 0 || s.Offset != 0 {
		t.Fatalf("the child's state is %+v", s)
	}
	c.write(len(c.lines))
	c.hook(postInvocation, begin("0"))
	c.hook(stop, stopped)
	calls := c.drain()[semconv.TermaModelCallEvent]
	if len(calls) != 4 {
		t.Fatalf("child sent %d calls, want 4", len(calls))
	}
	for _, e := range calls {
		if e.SessionID != childConversation || e.Attrs[semconv.TermaTurnIDKey] != "turn-0" {
			t.Errorf("child's call under %s in %v", e.SessionID, e.Attrs[semconv.TermaTurnIDKey])
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
	g.hook(preInvocation, begin("1"))
	g.hook(stop, stopped)
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

// Tool output that quotes invoke_subagent's result links nothing.
func TestAntigravityQuotedSubagentResultLinksNothing(t *testing.T) {
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	// The fixture's subagent result, as the output of a command that printed it.
	quoted := g.lines[2]
	g.lines = []string{g.lines[0], strings.Replace(subagentCall1, `{"name":"invoke_subagent","args":{}},`, "", 1), quoted}
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(3)
	g.hook(stop, stopped)
	got := g.drain()
	if starts := linked(got); len(starts) != 0 {
		t.Fatalf("a quoted result linked a subagent: %v", starts)
	}
	if s := strings.Join(callSteps(got[semconv.TermaModelCallEvent]), ","); s != "step-1" {
		t.Fatalf("the call itself: %s", s)
	}
}

// linked is the session starts that link a child to its parent.
func linked(got map[string][]spool.Event) []spool.Event {
	var out []spool.Event
	for _, e := range got[semconv.TermaSessionStartEvent] {
		if _, ok := e.Attrs[semconv.TermaSessionParentIDKey]; ok {
			out = append(out, e)
		}
	}
	return out
}

// A call that asks for a subagent and another tool: only invoke_subagent's own result
// links a child, whatever the other tool's output quotes.
func TestAntigravitySubagentLinkIsTheInvokeSubagentResult(t *testing.T) {
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	other := strings.ReplaceAll(strings.Replace(g.lines[2], `"step_index":2`, `"step_index":3`, 1), childConversation, turnsConversation)
	g.lines = []string{g.lines[0], subagentCall1, g.lines[2], other}
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(4)
	g.hook(stop, stopped)
	starts := linked(g.drain())
	if len(starts) != 1 || starts[0].SessionID != childConversation {
		t.Fatalf("links: %v", starts)
	}
}

// A line too long to read is skipped, and what follows it is read.
func TestAntigravityOverlongLineIsSkipped(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	long := `{"step_index":1,"source":"MODEL","type":"GENERIC","status":"DONE","content":"` + strings.Repeat("x", antigravityLineLimit) + `"}`
	g.lines = []string{g.lines[0], long, strings.Replace(g.lines[1], `"step_index":1`, `"step_index":2`, 1)}
	g.write(3)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-2 turn-1" {
		t.Fatalf("sent %s", s)
	}
}
