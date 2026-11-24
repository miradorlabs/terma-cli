package antigravity

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func jsonUnmarshalInto(raw string, in *antigravityHookInput) error {
	return json.Unmarshal([]byte(`{"conversationId":"c1","toolCall":`+raw+`}`), in)
}

// captureAntigravityCalls is one hold of the conversation that only reads the transcript, as
// a hook of the record's turn would: whether it read it.
func captureAntigravityCalls(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput) bool {
	conv := openAntigravityConversation(ctx, e, r, in)
	defer conv.close()
	return conv.read(ctx)
}

// readCall is a tool call of a call line: read_file of path.
func readCall(path string) string {
	return `{"name":"read_file","args":{"Path":"` + hookruntest.InJSON(path) + `"}}`
}

// withCalls is a fixture's call line asking for calls, each a JSON tool call.
func withCalls(line string, calls ...string) string {
	i := strings.Index(line, `"tool_calls":`)
	if i < 0 {
		return strings.TrimSuffix(strings.TrimSpace(line), "}") + `,"tool_calls":[` + strings.Join(calls, ",") + "]}"
	}
	return line[:i] + `"tool_calls":[` + strings.Join(calls, ",") + "]}"
}

// callLine is a call line at step asking for calls, with no reply.
func callLine(step int, calls ...string) string {
	return withCalls(`{"step_index":`+strconv.Itoa(step)+`,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:47Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1}`, calls...)
}

// resultLine is a tool's result at step, filled with made-up text.
func resultLine(step int) string {
	return `{"step_index":` + strconv.Itoa(step) + `,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-20T16:51:48Z","content":"<scrubbed>"}`
}

// replyLine is a call line at step with a reply and no calls.
func replyLine(step int) string {
	return `{"step_index":` + strconv.Itoa(step) + `,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:49Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1,"content":"<scrubbed>"}`
}

// readTurn runs turn one with its step 2 a read of path, and the rest as turnOne.
func (g *transcriptRig) readTurn(path string) {
	g.t.Helper()
	g.lines[1] = withCalls(g.lines[1], readCall(path))
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	g.hook(postToolUse, toolUse(2, "read_file", `{"Path":"`+hookruntest.InJSON(path)+`"}`))
	g.write(4)
	g.hook(postToolUse, toolUse(4, "run_command", `{"CommandLine":"made-up command"}`))
	g.write(7)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
}

// stepContent reports whether step 2's call went with its arguments and its result went.
func stepContent(got map[string][]spool.Event, id string) (args, result bool) {
	_, args = byID(got[semconv.TermaToolCallEvent])[id].Attrs[semconv.GenAIToolCallArgumentsKey]
	_, result = byID(got[semconv.TermaToolResultEvent])[id]
	return args, result
}

// excludedFile is a file in a checkout the rig's policy does not collect.
func excludedFile(t *testing.T) string {
	t.Helper()
	excluded := hookruntest.InitRepo(t)
	hookruntest.WriteFile(t, excluded, "file.txt", "made-up\n")
	return filepath.Join(excluded, "file.txt")
}
