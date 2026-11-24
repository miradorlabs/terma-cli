package antigravity

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// What terma sends for each kind of transcript line, under each condition that decides it:
// the place to look. A cell lists the line's events in the order they leave, each with the
// content it carries: +text is terma.message.text, +result gen_ai.tool.call.result. An empty
// cell sends nothing. Tool calls and their arguments come from PostToolUse, not from a line,
// and are not here.
//
// The transcript is one turn: the prompt (step 0), a call line asking for a clean call (1)
// and its result (2), a call line asking for a read of an excluded checkout's file (3) and
// its result (4), a reply that asks for no tool (5), and a GENERIC line after it (6), outside
// any batch.
func TestAntigravityWhatEachLineSends(t *testing.T) {
	type sends struct {
		prompt     string // USER_INPUT
		reply      string // PLANNER_RESPONSE with content
		call       string // PLANNER_RESPONSE with tool calls
		clean      string // GENERIC after a clean batch
		refused    string // GENERIC after a refused batch
		outsideAny string // GENERIC outside a batch
	}
	for _, tc := range []struct {
		name string
		// set changes the rig from everything on: the policy's switches, the agents chosen, the
		// workspaces.
		set func(g *transcriptRig, excluded string)
		// elsewhere reads the transcript from a hook whose turn this is not: one in another
		// collected checkout.
		elsewhere bool
		want      sends
	}{
		{name: "everything on", want: sends{
			prompt: "terma.user.message +text", reply: "terma.model_call; terma.assistant.message +text",
			call: "terma.model_call", clean: "terma.tool.result +result",
		}},
		{name: "prompts off", set: func(g *transcriptRig, _ string) { g.noPrompts = true }, want: sends{
			prompt: "terma.user.message", reply: "terma.model_call",
			call: "terma.model_call", clean: "terma.tool.result +result",
		}},
		{name: "tool content off", set: func(g *transcriptRig, _ string) { g.noToolContent = true }, want: sends{
			prompt: "terma.user.message +text", reply: "terma.model_call; terma.assistant.message +text",
			call: "terma.model_call",
		}},
		{name: "Antigravity not chosen", set: func(g *transcriptRig, _ string) { g.agents = []string{"codex"} }, want: sends{
			prompt: "terma.user.message", reply: "terma.model_call", call: "terma.model_call",
		}},
		{name: "a workspace not collected", set: func(g *transcriptRig, excluded string) { g.workspaces = []string{g.root, excluded} }, want: sends{
			prompt: "terma.user.message", reply: "terma.model_call", call: "terma.model_call",
		}},
		{name: "not the hook's own turn", elsewhere: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := excludedFile(t)
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			if tc.set != nil {
				tc.set(g, filepath.Dir(file))
			}
			g.lines = []string{g.lines[0], callLine(1, `{"name":"run_command","args":{"CommandLine":"made-up"}}`), resultLine(2),
				callLine(3, readCall(file)), resultLine(4), replyLine(5), resultLine(6)}
			if tc.elsewhere {
				// The turn begins here, before any line; its lines are read in another checkout.
				g.hook(preInvocation, begin("1"))
				other := hookruntest.InitRepo(t)
				g.admitted = append(g.admitted, other)
				g.write(len(g.lines))
				g.hookAt(stop, other, stopped)
			} else {
				g.write(1)
				g.hook(preInvocation, begin("1"))
				g.write(len(g.lines))
				g.hook(stop, stopped)
			}
			g.drain()
			if tc.elsewhere && g.lost() != 3 {
				t.Errorf("the other checkout passed %d calls, want the 3 it read", g.lost())
			}
			got := sends{
				prompt: lineSends(g.seen, 0), call: lineSends(g.seen, 1), clean: lineSends(g.seen, 2),
				refused: lineSends(g.seen, 4), reply: lineSends(g.seen, 5), outsideAny: lineSends(g.seen, 6),
			}
			if lineSends(g.seen, 3) != tc.want.call {
				t.Errorf("the refused batch's call line: %q, want %q", lineSends(g.seen, 3), tc.want.call)
			}
			if got != tc.want {
				t.Errorf("sent\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

// lineSends is the events of one step, in the order they were stamped, each named with the
// content it carries.
func lineSends(events []spool.Event, step int64) string {
	content := map[string]string{semconv.TermaMessageTextKey: "+text", semconv.GenAIToolCallResultKey: "+result"}
	var out []string
	for _, e := range events {
		if norm(e.Attrs[semconv.TermaStepIndexKey]) != norm(step) || e.Name == semconv.TermaSessionStartEvent {
			continue
		}
		cell := e.Name
		for _, k := range []string{semconv.TermaMessageTextKey, semconv.GenAIToolCallResultKey} {
			if _, ok := e.Attrs[k]; ok {
				cell += " " + content[k]
			}
		}
		out = append(out, cell)
	}
	return strings.Join(out, "; ")
}
