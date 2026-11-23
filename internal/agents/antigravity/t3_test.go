package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// testdata/t3_acp_hooks.jsonl is every hook payload of a two-turn T3 Code thread run by its
// bundled antigravity 1.5.14 in ACP mode (gemini-4-neon-low, 2026-11-23), the prompts cut
// and the home folder's path replaced by /home/dev; the thread worked in t3Worktree.
// testdata/t3_acp_transcript.jsonl is that conversation's transcript, each step's index,
// source, type, status, time, model, tokens and tool names. Line i is step i.
// testdata/t3_acp_transcript_content.jsonl is a copy with made-up content in the real
// shapes: each prompt in agy's envelope around T3 Code's wrapper, each tool's result, and
// each turn's reply.
const (
	t3Conversation = "4f56f53a-9318-4a8a-8136-5c8bd563b2ee"
	t3Worktree     = "/home/dev/.t3/worktrees/terma-cli/t3-2c0e1164-91bdb0d41c8fe828"
	t3Provider     = "/home/dev/.t3/userdata/providers/antigravity/ac0a3dfd6dddb20962cecff6ee5fe65e19d3923be20e52c5ab52ff877f7e4c32"
)

// t3Payload is the part of a T3 payload the replay reads to grow the transcript.
type t3Payload struct {
	Event           string `json:"hookEventName"`
	InitialNumSteps int    `json:"initialNumSteps"`
	StepIdx         int    `json:"stepIdx"`
}

// T3 Code's Antigravity runs terma's hooks from its own home, in the thread's worktree: the
// turn's tool steps and each model call, with its tokens, are sent as from agy's own home.
// With Antigravity chosen and content in the transcript, each prompt goes with T3's wrapper
// off, each call with its arguments, each result and each reply.
func TestAntigravityT3CodeThreadSendsItsTurns(t *testing.T) {
	t.Run("as captured", func(t *testing.T) { replayT3(t, "t3_acp_transcript.jsonl", nil) })
	t.Run("with made-up content", func(t *testing.T) {
		got := replayT3(t, "t3_acp_transcript_content.jsonl", []string{Agent{}.Name()})
		var prompts []string
		for _, e := range hookruntest.Named(got, semconv.TermaUserMessageEvent) {
			prompts = append(prompts, e.Attrs[semconv.TermaMessageIDKey].(string)+" "+e.Attrs[semconv.TermaTurnIDKey].(string)+" "+e.Attrs[semconv.TermaMessageTextKey].(string))
		}
		want := []string{
			"step-0 turn-1 [T3 Code: the agent in T3 thread \"made-up\" sent this message, not the user.]\n\nAct as a made-up sub-agent.\n\nWrite alpha to a made-up file, then change it.",
			"step-14 turn-15 [T3 Code: the agent in T3 thread \"made-up\" sent this message, not the user.]\nNow read the made-up file back.",
		}
		if strings.Join(prompts, "|") != strings.Join(want, "|") {
			t.Errorf("prompts\n got %q\nwant %q", prompts, want)
		}
		var replies []string
		for _, e := range hookruntest.Named(got, semconv.TermaAssistantMessageEvent) {
			replies = append(replies, e.Attrs[semconv.TermaMessageIDKey].(string)+" "+e.Attrs[semconv.TermaTurnIDKey].(string)+" "+e.Attrs[semconv.TermaMessageTextKey].(string))
		}
		if strings.Join(replies, "|") != "step-13 turn-1 Made-up reply at step 13.|step-21 turn-15 Made-up reply at step 21." {
			t.Errorf("replies %q", replies)
		}
		calls := map[string]time.Time{}
		for _, e := range hookruntest.Named(got, semconv.TermaToolCallEvent) {
			id := e.Attrs[semconv.GenAIToolCallIDKey].(string)
			calls[id] = e.Time
			if args, ok := e.Attrs[semconv.GenAIToolCallArgumentsKey].(map[string]any); !ok || len(args) == 0 {
				t.Errorf("%s went without its arguments: %v", id, e.Attrs)
			}
		}
		var results []string
		for _, e := range hookruntest.Named(got, semconv.TermaToolResultEvent) {
			id := e.Attrs[semconv.GenAIToolCallIDKey].(string)
			results = append(results, id)
			if text, _ := e.Attrs[semconv.GenAIToolCallResultKey].(string); !strings.HasSuffix(text, "made-up output of "+strings.Replace(id, "-", " ", 1)) {
				t.Errorf("%s's result: %q", id, text)
			}
			if at, ok := calls[id]; !ok || !e.Time.After(at) {
				t.Errorf("%s's result has no call before it", id)
			}
		}
		if s := strings.Join(results, " "); s != "step-2 step-4 step-7 step-9 step-12 step-17 step-19" {
			t.Errorf("results %s", s)
		}
	})
}

// replayT3 runs the capture's hooks against the transcript fixture as it grew, with agents
// chosen, checks what is sent today, and returns every event spooled.
func replayT3(t *testing.T, fixture string, agents []string) []spool.Event {
	t.Helper()
	root := hookruntest.InitRepo(t)
	home := isolatedHome(t)
	provider := filepath.Join(home, ".t3", "userdata", "providers", "antigravity", filepath.Base(t3Provider))
	transcript := filepath.Join(provider, t3AppDataDir, "brain", t3Conversation, ".system_generated", "logs", antigravityTranscriptName)
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	steps, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(steps), "\n")
	lines = lines[:len(lines)-1]
	data, err := os.ReadFile(filepath.Join("testdata", "t3_acp_hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.NewReplacer(t3Worktree, hookruntest.InJSON(root), "/home/dev", hookruntest.InJSON(home)).Replace(string(data))
	var payloads []string
	var parsed []t3Payload
	sc := bufio.NewScanner(strings.NewReader(moved))
	for sc.Scan() {
		var p t3Payload
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		payloads, parsed = append(payloads, sc.Text()), append(parsed, p)
	}
	if len(payloads) != 27 {
		t.Fatalf("the capture has %d payloads, want 27", len(payloads))
	}

	// upTo is how many steps are on disk when payload i runs: a turn's model call waits for
	// PreInvocation, and a step's tool result is written only once its PostToolUse hooks have
	// returned (seen on T3 Code's 1.5.14 and agy 1.5.15 with a hook that slept 3 s, for read,
	// write, edit, command and MCP tools).
	upTo := func(i int) int {
		switch p := parsed[i]; p.Event {
		case "PreInvocation":
			return p.InitialNumSteps
		case "PostToolUse":
			return p.StepIdx
		}
		// PostInvocation and Stop: what the next invocation finds, less the next turn's
		// prompt after a Stop.
		stopped := false
		for _, next := range parsed[i+1:] {
			if next.Event == "Stop" {
				stopped = true
			}
			if next.Event == "PreInvocation" {
				if stopped || parsed[i].Event == "Stop" {
					return next.InitialNumSteps - 1
				}
				return next.InitialNumSteps
			}
		}
		return len(lines)
	}
	handlers := map[string]func(context.Context, hookrun.Env) error{
		"PreInvocation": preInvocation, "PostToolUse": postToolUse, "PostInvocation": postInvocation, "Stop": stop,
	}
	sp, _ := hookruntest.OpenSpool(t.TempDir())
	stateDir := t.TempDir()
	hookDir := filepath.Join(provider, "config")
	for i, p := range payloads {
		if err := os.WriteFile(transcript, []byte(strings.Join(lines[:upTo(i)], "")), 0o600); err != nil {
			t.Fatal(err)
		}
		if parsed[i].Event == "PostToolUse" && parsed[i].StepIdx == 4 {
			hookruntest.WriteFile(t, root, "probe17.txt", "alpha\n")
		}
		var stdout bytes.Buffer
		env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: hookDir, Policy: hookruntest.Admitting(root), Agents: agents, Stdin: strings.NewReader(p), Stdout: &stdout, Spool: sp, Version: "test"}
		if err := handlers[parsed[i].Event](context.Background(), env); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(stdout.String()) != "{}" {
			t.Fatalf("payload %d: agy expects {} on stdout, got %q", i, stdout.String())
		}
	}

	var tools, calls []string
	var input, output int64
	touched := 0
	spooled := hookruntest.Spooled(t, sp)
	for _, e := range spooled {
		if e.SessionID != t3Conversation {
			t.Errorf("%s for session %q", e.Name, e.SessionID)
		}
		switch e.Name {
		case semconv.TermaToolCallEvent:
			tools = append(tools, e.Attrs[semconv.GenAIToolNameKey].(string)+"/"+e.Attrs[semconv.GenAIToolCallIDKey].(string)+"/"+
				e.Attrs[semconv.TermaTurnIDKey].(string)+"/"+e.Attrs[semconv.TermaOperationStatusKey].(string))
		case semconv.TermaModelCallEvent:
			calls = append(calls, e.Attrs[semconv.TermaModelCallIDKey].(string)+"/"+e.Attrs[semconv.TermaTurnIDKey].(string))
			in, _ := norm(e.Attrs[semconv.GenAIUsageInputTokensKey]).(float64)
			out, _ := norm(e.Attrs[semconv.GenAIUsageOutputTokensKey]).(float64)
			input, output = input+int64(in), output+int64(out)
			if e.Attrs[semconv.GenAIRequestModelKey] != "gemini-4-neon-low" {
				t.Errorf("model call %v", e.Attrs)
			}
		case semconv.TermaFilesTouchedEvent:
			touched++
			if hookruntest.Joined(e.Attrs[semconv.TermaFilesPathsKey]) != "probe17.txt" {
				t.Errorf("files touched: %v", e.Attrs)
			}
		}
	}
	wantTools := []string{
		"read_file/step-2/turn-1/completed", "write_file/step-4/turn-1/completed", "edit_file/step-7/turn-1/completed",
		"run_command/step-9/turn-1/completed", "read_file/step-12/turn-1/error",
		"run_command/step-17/turn-15/completed", "run_command/step-19/turn-15/completed",
	}
	if strings.Join(tools, " ") != strings.Join(wantTools, " ") {
		t.Errorf("tool calls\n got %v\nwant %v", tools, wantTools)
	}
	wantCalls := []string{
		"step-1/turn-1", "step-3/turn-1", "step-6/turn-1", "step-8/turn-1", "step-11/turn-1", "step-13/turn-1",
		"step-16/turn-15", "step-18/turn-15", "step-21/turn-15",
	}
	if strings.Join(calls, " ") != strings.Join(wantCalls, " ") {
		t.Errorf("model calls\n got %v\nwant %v", calls, wantCalls)
	}
	// Each call's input counts what it read from the cache; the transcript's sums.
	var wantIn, wantOut int64
	for _, l := range lines {
		var s struct{ InputTokens, CacheReadTokens, OutputTokens int64 }
		var raw map[string]json.RawMessage
		_ = json.Unmarshal([]byte(l), &raw)
		_ = json.Unmarshal(raw["input_tokens"], &s.InputTokens)
		_ = json.Unmarshal(raw["cache_read_tokens"], &s.CacheReadTokens)
		_ = json.Unmarshal(raw["output_tokens"], &s.OutputTokens)
		wantIn, wantOut = wantIn+s.InputTokens+s.CacheReadTokens, wantOut+s.OutputTokens
	}
	if input != wantIn || output != wantOut {
		t.Errorf("tokens in %d out %d, want %d and %d", input, output, wantIn, wantOut)
	}
	if touched == 0 {
		t.Error("the files its file tools wrote were not recorded")
	}
	return spooled
}
