package antigravity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The transcript as events (the package comment says how it fits): where a conversation's
// transcript is, a hook's read of it, and what one line sends. agy writes each model call as a
// PLANNER_RESPONSE line, with its model and token counts, before the hooks that follow the
// call run, and beside them the prompt (USER_INPUT), the reply (a PLANNER_RESPONSE's content)
// and each tool's result (GENERIC). The highest step handled keeps any line from going twice,
// even after compaction rewrites the file.

// antigravityTranscriptName is the transcript that holds the whole of every step; the
// compact transcript.jsonl beside it cuts long fields.
const antigravityTranscriptName = "transcript_full.jsonl"

// antigravityCallDir holds how far each conversation's transcript has been read.
var antigravityCallDir = hookrun.AgentStateDir(Agent{}.Name(), "calls")

// antigravityCallLockWait bounds the wait for another hook of the conversation reading the
// same transcript; past it the calls wait for the next hook.
const antigravityCallLockWait = 3 * time.Second

// antigravityTranscript is the hook's transcriptPath when it is the conversation's own
// transcript in agy's brain folder: the full one, never the compact copy.
func antigravityTranscript(in *antigravityHookInput) string {
	p := in.TranscriptPath
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if filepath.Base(p) == "transcript.jsonl" {
		p = filepath.Join(filepath.Dir(p), antigravityTranscriptName)
	}
	if filepath.Base(p) != antigravityTranscriptName {
		return ""
	}
	// <appDataDir>/brain/<conversationId>/.system_generated/logs/transcript_full.jsonl
	logs := filepath.Dir(p)
	generated := filepath.Dir(logs)
	conversation := filepath.Dir(generated)
	if filepath.Base(logs) != "logs" || filepath.Base(generated) != ".system_generated" ||
		filepath.Base(conversation) != in.id() || filepath.Base(filepath.Dir(conversation)) != "brain" {
		return ""
	}
	return p
}

// read reads the transcript past the last read and sends each line the rule in turn.go lets
// this hook send, through send below; it passes the rest. It reports whether it read the
// transcript, so the hook's observation says usage travels apart. Without the lock it reads
// nothing.
func (c *antigravityConversation) read(ctx context.Context) bool {
	if !c.held {
		return false
	}
	e, r, id := c.e, c.r, c.in.id()
	// The turn this hook saw begin here, and its first step; -1 sends nothing.
	own := antigravityTurnID(e, r, id)
	first := antigravityTurnNumber(own)
	prompt := int64(-1)
	if first > 0 {
		prompt = first - 1
	}
	passed := 0
	pass := func(s antigravityStep) bool {
		if s.Type == "PLANNER_RESPONSE" {
			passed++
		}
		return true
	}
	// A call is refused by the rule its hook judges its own arguments by.
	next, err := readAntigravitySteps(ctx, antigravityTranscript(c.in), c.state, prompt, c.refuses, func(s antigravityStep) bool {
		if first < 0 || (*s.StepIndex < first && !s.prompt) {
			// Before this turn, or no turn of this hook's: passed for good. The one line before
			// the turn that is its own is the prompt it began with.
			return pass(s)
		}
		// The record must still name this turn right before the send. That is enough: agy
		// runs PreInvocation before it calls the model (on 1.5.15, a hook that slept 3 s
		// delayed the call by 3 s), so a later turn's first hook rewrites or removes the
		// record before that turn's first call is written. A line read while the record still
		// names this turn is this turn's, or an earlier one's.
		if antigravityTurnID(e, r, id) != own {
			first = -1
			return pass(s)
		}
		return c.send(s, own)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.Logf("antigravity transcript: %v", err)
	}
	if passed > 0 {
		e.CountLost(r, sendas.LostTurnEnded, passed)
	}
	// The clock moved as the read's events were stamped.
	next.Clock = c.state.Clock
	c.state = next
	return err == nil
}

// send spools what one line of this hook's own turn holds: a prompt, a model call and its
// reply, or a tool's result and the subagent it opened. Content goes only with its kind's
// consent (antigravityContentFor); a result of a tainted batch does not go, nor a GENERIC
// line outside a batch, which is no tool's result. It reports false when something could not
// be spooled, for the line to be read again.
func (c *antigravityConversation) send(s antigravityStep, turn string) bool {
	e, r, id := c.e, c.r, c.in.id()
	at := antigravityLineTime(s, e.Time())
	switch s.Type {
	case "USER_INPUT":
		// DESIGN: design 3.3 says a hook whose workspaces are not all collected "sends what it
		// sends today and no content". The prompt event is new, and goes here without its text,
		// as it does with prompts withheld (3.1), so the prompt still counts; it carries no
		// content. Sending no prompt event at all would be the stricter reading.
		ev := antigravityPromptEvent(id, s, turn, c.contentFor().prompts)
		ev.Time = c.stamp(at)
		return e.EmitFor(r, ev)
	case "PLANNER_RESPONSE":
		ev := antigravityCallEvent(id, s, turn)
		ev.Time = c.stamp(at)
		if !e.EmitFor(r, ev) {
			return false
		}
		// A reply after a refused read goes, as every agent's does; withholding it would need
		// the tainted call line's step kept in the state.
		if s.Content == "" || !c.contentFor().prompts {
			return true
		}
		reply := antigravityReplyEvent(id, s, turn)
		reply.Time = c.stamp(at)
		return e.EmitFor(r, reply)
	case "GENERIC":
		c.call.meet(*s.StepIndex, func() time.Time { return c.stamp(at) })
		// Only invoke_subagent's result names a child; other output could quote one. The link
		// is not content: it goes whether or not the batch is tainted.
		if child := antigravitySubagent(s.Content); s.subagentResult && child != "" && child != id {
			// The child's own hooks name no parent: its session start, again, does.
			link := spool.Event{Name: semconv.TermaSessionStartEvent, SessionID: child, Time: c.stamp(at), Attrs: map[string]any{
				semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaSessionParentIDKey: id,
			}}
			if !e.EmitFor(r, link) {
				return false
			}
		}
		if !s.toolResult || s.withheld || s.Content == "" || !c.contentFor().tools {
			return true
		}
		ev := antigravityResultEvent(id, s, turn)
		ev.Time = c.stamp(at)
		return e.EmitFor(r, ev)
	}
	return true
}
