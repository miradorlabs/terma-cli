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
)

// testdata/t3_acp_hooks.jsonl is every hook payload of a two-turn T3 Code thread run by its
// bundled antigravity 1.5.14 in ACP mode (gemini-4-neon-low, 2026-11-23), the prompts cut
// and the home folder's path replaced by /home/dev; the thread worked in t3Worktree.
// testdata/t3_acp_transcript.jsonl is that conversation's transcript, each step's index,
// source, type, status, time, model, tokens and tool names. Line i is step i.
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
func TestAntigravityT3CodeThreadSendsItsTurns(t *testing.T) {
	root := hookruntest.InitRepo(t)
	home := isolatedHome(t)
	provider := filepath.Join(home, ".t3", "userdata", "providers", "antigravity", filepath.Base(t3Provider))
	transcript := filepath.Join(provider, t3AppDataDir, "brain", t3Conversation, ".system_generated", "logs", antigravityTranscriptName)
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	steps, err := os.ReadFile(filepath.Join("testdata", "t3_acp_transcript.jsonl"))
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
	// PreInvocation, and a step's tool result is written before its PostToolUse.
	upTo := func(i int) int {
		switch p := parsed[i]; p.Event {
		case "PreInvocation":
			return p.InitialNumSteps
		case "PostToolUse":
			return p.StepIdx + 1
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
		env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: hookDir, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(p), Stdout: &stdout, Spool: sp, Version: "test"}
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
	for _, e := range hookruntest.Spooled(t, sp) {
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
}
