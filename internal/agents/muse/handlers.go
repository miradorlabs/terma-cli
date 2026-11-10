package muse

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// sourceMuseHook is terma.evidence.source for what a Muse hook reported.
const sourceMuseHook = "muse_hook"

// museRootEnv is the root session's id, which Muse sets in every hook's environment; a
// subagent's hooks name the child in session_id.
const museRootEnv = "MUSE_SESSION_ID"

// museMaxText bounds a prompt or reply; a longer one is almost always a pasted file.
const museMaxText = 16 << 10

// museMaxArguments bounds the tool input a terma.tool.call carries: a whole file's write is
// content the record need not repeat.
const museMaxArguments = 16 << 10

// museHookInput is the JSON Muse writes to a hook's stdin, the fields terma reads. Every
// event names its session, directory and model; the rest are the event's own.
type museHookInput struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	TurnID    string `json:"turn_id"`
	// SessionStart's startup or resume; SessionEnd's reason.
	Source string `json:"source"`
	// UserPromptSubmit.
	Prompt string `json:"prompt"`
	// PostToolUse and PostToolUseFailure; the response is never opened.
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
	// PostLLMCall; the call's messages and tools are never opened.
	RequestID    string `json:"request_id"`
	ResponseID   string `json:"response_id"`
	FinishReason string `json:"finish_reason"`
	Status       string `json:"status"`
	Step         *int64 `json:"step"`
	Usage        *struct {
		InputTokens     *int64 `json:"input_tokens"`
		OutputTokens    *int64 `json:"output_tokens"`
		CachedTokens    *int64 `json:"cached_tokens"`
		CacheReadTokens *int64 `json:"cache_read_tokens"`
		CacheWrite      *int64 `json:"cache_write_tokens"`
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"usage"`
	// Stop.
	LastAssistantMessage string `json:"last_assistant_message"`
	// SubagentStart and SubagentStop.
	SubagentID     string `json:"subagent_id"`
	ChildSessionID string `json:"child_session_id"`
	SubagentType   string `json:"subagent_type"`
	AgentType      string `json:"agent_type"`
}

const museTool = name

// ids are the root session's id and, inside a subagent, the thread's: the root from the
// environment Muse sets, else the payload's session.
func (in *museHookInput) ids() (root, thread string) {
	root = in.SessionID
	if env := os.Getenv(museRootEnv); session.ValidID(env) {
		root = env
	}
	if !session.ValidID(root) {
		return "", ""
	}
	if in.SessionID != root && session.ValidID(in.SessionID) {
		thread = in.SessionID
	}
	return root, thread
}

// model is the payload's model, which Muse reports as "unknown" when it has none.
func (in *museHookInput) model() string {
	if in.Model == "unknown" {
		return ""
	}
	return in.Model
}

func readMuseInput(r io.Reader) (*museHookInput, error) {
	in, err := hookrun.ReadInput[museHookInput](r)
	if err != nil {
		return nil, err
	}
	if root, _ := in.ids(); root == "" {
		return nil, errors.New("hook input has no safe session_id")
	}
	return in, nil
}

// open reads the payload and resolves the root session's repository from its directory.
func open(ctx context.Context, env *hookrun.Env) (*museHookInput, *hookrun.Repo, bool) {
	in, err := readMuseInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil, nil, false
	}
	root, _ := in.ids()
	r, ok := env.Open(ctx, root, in.Cwd)
	return in, r, ok
}

// attrs opens a record of the hook: the thread, when a subagent's, and the turn.
func (in *museHookInput) attrs(hook string) map[string]any {
	a := hookrun.EvidenceAttrs(museTool, sourceMuseHook, hook)
	_, thread := in.ids()
	a = hookrun.AgentAttrs(a, thread, "")
	hookrun.BoundedAttr(a, semconv.TermaTurnIDKey, in.TurnID)
	hookrun.BoundedAttr(a, semconv.GenAIRequestModelKey, in.model())
	return a
}

// consented reports whether what was said may be spooled at all: Muse is among the
// developer's agents, or global mode collects every session. The team's policy decides,
// when it leaves, whether prompts may.
func consented(c hookrun.Consent) bool { return c.Global || slices.Contains(c.Agents, name) }

func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, r, ok := open(ctx, &env)
	if !ok {
		return nil
	}
	root, thread := in.ids()
	if thread != "" {
		return nil
	}
	env.Announce(r, env.NewSession(r, root, museTool, in.model()), nil)
	return nil
}

// prompt keeps the session active, as the hook before each turn, and spools what the
// person asked.
func prompt(ctx context.Context, env hookrun.Env) error {
	in, r, ok := open(ctx, &env)
	if !ok {
		return nil
	}
	root, thread := in.ids()
	if thread == "" {
		sess := env.NewSession(r, root, museTool, in.model())
		if active, _ := r.Store.Active(sess.UpdatedAt, 0); active != nil && active.ID == root && !active.StartedAt.IsZero() {
			sess.StartedAt = active.StartedAt
		}
		env.SetActive(r, sess)
	}
	spoolMessage(env, r, in, semconv.TermaUserMessageEvent, "UserPromptSubmit", in.Prompt, "prompt")
	return nil
}

// spoolMessage spools a prompt or reply under the developer's consent, cut to museMaxText
// on a rune boundary. Its id is the turn's, so a hook that fires twice sends one message.
func spoolMessage(env hookrun.Env, r *hookrun.Repo, in *museHookInput, event, hook, text, kind string) {
	if strings.TrimSpace(text) == "" || !consented(env.Consent()) {
		return
	}
	root, _ := in.ids()
	a := in.attrs(hook)
	delete(a, semconv.TermaHookEventKey)
	a[semconv.TermaMessageIDKey] = hookrun.EvidenceID(root + "\x00" + cmp.Or(in.TurnID, env.Time().String()) + "\x00" + kind)
	a[semconv.TermaMessageTruncatedKey] = false
	if len(text) > museMaxText {
		cut := museMaxText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text, a[semconv.TermaMessageTruncatedKey] = text[:cut], true
	}
	a[semconv.TermaMessageTextKey] = text
	env.EmitFor(r, spool.Event{Name: event, SessionID: root, Attrs: a})
}

// postToolUse spools each tool call, its input as tool content, and the files an edit
// changed for the commit that follows.
func postToolUse(ctx context.Context, env hookrun.Env) error {
	in, r, ok := open(ctx, &env)
	if !ok || !hookrun.ShortLabel(in.ToolName) {
		return nil
	}
	root, _ := in.ids()
	a := in.attrs(cmp.Or(in.Event, "PostToolUse"))
	a[semconv.GenAIToolNameKey] = in.ToolName
	hookrun.BoundedAttr(a, semconv.GenAIToolCallIDKey, in.ToolUseID)
	a[semconv.TermaOperationStatusKey] = "completed"
	if in.Event == "PostToolUseFailure" {
		a[semconv.TermaOperationStatusKey] = "error"
	}
	if len(in.ToolInput) > 0 && len(in.ToolInput) <= museMaxArguments {
		var args any
		if json.Unmarshal(in.ToolInput, &args) == nil {
			a[semconv.GenAIToolCallArgumentsKey] = args
		}
	}
	env.EmitFor(r, spool.Event{Name: semconv.TermaToolCallEvent, SessionID: root, Attrs: a})
	if a[semconv.TermaOperationStatusKey] != "completed" {
		return nil
	}
	if files := editedPaths(in.ToolName, in.ToolInput); len(files) > 0 {
		ids := map[string]any{}
		for _, k := range []string{semconv.GenAIToolCallIDKey, semconv.TermaTurnIDKey, semconv.TermaAgentIDKey} {
			if v, ok := a[k]; ok {
				ids[k] = v
			}
		}
		env.Touch(r, session.Session{ID: root, Tool: museTool, Model: in.model()}, in.ToolName, files, ids)
	}
	return nil
}

// museEditTools are the tools that change files; apply_patch names its files in the patch.
var museEditTools = map[string]bool{"write_file": true, "edit_file": true, "apply_patch": true}

var musePathKeys = []string{"path", "file_path", "target_file"}

// editedPaths are the files an edit tool's input names: its path, or a patch's file headers.
func editedPaths(tool string, input json.RawMessage) []string {
	if !museEditTools[tool] || len(input) == 0 {
		return nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return nil
	}
	var out []string
	for _, key := range musePathKeys {
		var p string
		if json.Unmarshal(args[key], &p) == nil && p != "" {
			out = append(out, p)
		}
	}
	if tool == "apply_patch" {
		for _, key := range []string{"input", "patch"} {
			var patch string
			if json.Unmarshal(args[key], &patch) == nil {
				out = append(out, patchPaths(patch)...)
			}
		}
	}
	return out
}

// patchPaths are the files a patch adds, updates, deletes or moves to.
func patchPaths(patch string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(patch))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
			if p, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(p) != "" {
				out = append(out, strings.TrimSpace(p))
			}
		}
	}
	return out
}

// postLLMCall spools one model call with the usage Muse reported for it, a subagent's and
// Muse's own reminders' included: each is a call the account pays for.
func postLLMCall(ctx context.Context, env hookrun.Env) error {
	in, r, ok := open(ctx, &env)
	if !ok {
		return nil
	}
	root, _ := in.ids()
	a := in.attrs("PostLLMCall")
	delete(a, semconv.TermaHookEventKey)
	hookrun.BoundedAttr(a, semconv.TermaModelCallIDKey, cmp.Or(in.RequestID, in.ResponseID))
	if a[semconv.TermaModelCallIDKey] == nil {
		return nil
	}
	hookrun.BoundedAttr(a, semconv.GenAIResponseIDKey, in.ResponseID)
	if hookrun.ShortLabel(in.FinishReason) {
		a[semconv.GenAIResponseFinishReasonsKey] = []string{in.FinishReason}
	}
	if hookrun.ShortLabel(in.Status) {
		a[semconv.TermaOperationStatusKey] = in.Status
	}
	if in.Step != nil && *in.Step >= 0 {
		a[semconv.TermaStepIndexKey] = *in.Step
	}
	if u := in.Usage; u != nil {
		// cached_tokens is Muse's combined count; a build that splits reads from writes says so.
		cacheRead := u.CachedTokens
		if u.CacheReadTokens != nil {
			cacheRead = u.CacheReadTokens
		}
		for k, v := range map[string]*int64{
			semconv.GenAIUsageInputTokensKey: u.InputTokens, semconv.GenAIUsageOutputTokensKey: u.OutputTokens,
			semconv.GenAIUsageCacheReadInputTokensKey: cacheRead, semconv.GenAIUsageCacheWriteInputTokensKey: u.CacheWrite,
			semconv.GenAIUsageReasoningOutputTokensKey: u.ReasoningTokens,
		} {
			if v != nil && *v >= 0 {
				a[k] = *v
			}
		}
	}
	env.EmitFor(r, spool.Event{Name: semconv.TermaModelCallEvent, SessionID: root, Attrs: a})
	return nil
}

func subagentStart(ctx context.Context, env hookrun.Env) error {
	return subagent(ctx, env, semconv.TermaSubagentStartEvent)
}

func subagentStop(ctx context.Context, env hookrun.Env) error {
	return subagent(ctx, env, semconv.TermaSubagentEndEvent)
}

// subagent spools a run's start or end in the root session, the run named by its own session.
func subagent(ctx context.Context, env hookrun.Env, event string) error {
	in, r, ok := open(ctx, &env)
	if !ok {
		return nil
	}
	root, _ := in.ids()
	child := cmp.Or(in.ChildSessionID, in.SessionID)
	if child == root || !session.ValidID(child) {
		return nil
	}
	a := hookrun.AgentAttrs(map[string]any{semconv.GenAIMainAgentNameKey: museTool}, child, cmp.Or(in.SubagentType, in.AgentType, in.SubagentID))
	hookrun.BoundedAttr(a, semconv.TermaTurnIDKey, in.TurnID)
	env.EmitFor(r, spool.Event{Name: event, SessionID: root, Attrs: a})
	return nil
}

// stop keeps the session fresh for the commit that may follow and spools the turn's reply.
func stop(ctx context.Context, env hookrun.Env) error {
	in, r, ok := open(ctx, &env)
	if !ok {
		return nil
	}
	root, thread := in.ids()
	if thread == "" {
		now := env.Time()
		if active, _ := r.Store.Active(now, 0); active != nil && active.ID == root {
			active.UpdatedAt, active.Model = now, cmp.Or(in.model(), active.Model)
			_ = r.Store.SetActive(*active)
		}
	}
	spoolMessage(env, r, in, semconv.TermaAssistantMessageEvent, "Stop", in.LastAssistantMessage, "reply")
	if thread == "" {
		readLater(env, root, in.Cwd, in.TurnID)
	}
	return nil
}

// sessionEnd clears the active session and spools the end; manifests stay for uncommitted work.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, r, ok := open(ctx, &env)
	if !ok {
		return nil
	}
	root, thread := in.ids()
	if thread != "" {
		return nil
	}
	env.EndSession(r, root, museTool)
	readLater(env, root, in.Cwd, "")
	return nil
}
