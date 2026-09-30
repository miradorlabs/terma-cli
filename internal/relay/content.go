package relay

import (
	"slices"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The global exporters send content (the relay cannot tell a repository's policy
// before it sees the session), so the relay enforces each project's own: what its
// routing record withholds never leaves the machine. The sets are the difference
// between live/golden/<harness>/telemetry-content.json and telemetry-redacted.json,
// plus the prompt and reply fields both exports carry but blank.
var (
	// promptFields hold what was said. The harnesses blank them rather than drop
	// them when prompts are off, and the relay does the same, with each harness's own
	// marker, so the backend sees the shape it already parses.
	promptFields = []string{"prompt", "response", "user_prompt"}
	// promptDropFields hold what was said and are removed outright, as the exporters
	// that write them omit them when content is off: the GenAI semantic conventions'
	// content attributes (terma's OpenCode plugin writes gen_ai.completion).
	promptDropFields = []string{"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
		"gen_ai.tool.definitions",
		// omp's own (omp.gen_ai.*): the request's messages and the response's text.
		"omp.gen_ai.request.messages", "omp.gen_ai.response.text",
		// Gemini CLI's gemini_cli.api_request / api_response (0.62).
		"request_text", "response_text"}
	// resourcePromptFields are resource attributes that restate what was said: the
	// process's command line, which carries a prompt passed as an argument (Gemini CLI
	// stamps process.command_args, `-p "<prompt>"` included, whatever logPrompts says).
	resourcePromptFields = []string{"process.command_args", "process.command_line"}
	// promptBodyEvents carry what was said in the log body, not an attribute: the
	// OpenCode plugin's prompt, the session title, which restates it, Pi's prompt, and
	// Hermes's prompt and reply.
	promptBodyEvents = []string{"opencode.user_prompt", "opencode.session.created", "pi.user_prompt", "omp.user_prompt", "hermes.user_prompt", "hermes.assistant_response", "dsh.user_prompt", "dsh.assistant_response"}
	// toolContentFields hold what a tool was called with or returned.
	toolContentFields = []string{"tool_parameters", "tool_input", "full_command", "bash_command", "arguments", "output",
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "opencode.tool.file_path",
		// Gemini CLI's tool_call and hook_call records (0.62).
		"function_args", "hook_input", "hook_output", "stdout", "stderr"}
	// toolContentEvents are span events that exist only to carry tool content: Claude
	// Code's claude_code.tool span records the command and its output as a
	// tool.output event (live, 2.1.284), which the golden attribute lists do not see.
	toolContentEvents = []string{"tool.output", "tool.input"}
)

const (
	claudeRedacted = "<REDACTED>"
	codexRedacted  = "[REDACTED]"
)

// withhold applies a project's content policy to one session's part, in place, and
// returns how many records it changed.
func withhold(p *part, prompts, toolContent bool) int {
	if prompts && toolContent {
		return 0
	}
	changed := 0
	apply := func(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
		out, did := withholdAttrs(attrs, prompts, toolContent)
		if did {
			changed++
		}
		return out
	}
	resource := func(r *resourcepb.Resource) {
		if prompts || r == nil {
			return
		}
		kept := r.Attributes[:0]
		for _, kv := range r.GetAttributes() {
			if contains(resourcePromptFields, kv.GetKey()) {
				changed++
				continue
			}
			kept = append(kept, kv)
		}
		r.Attributes = kept
	}
	switch m := p.msg.(type) {
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			resource(rm.GetResource())
		}
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			resource(rl.GetResource())
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					lr.Attributes = apply(lr.GetAttributes())
					if !prompts && contains(promptBodyEvents, attrString(lr.GetAttributes(), "event.name")) && lr.GetBody().GetStringValue() != "" {
						lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ""}}
						changed++
					}
				}
			}
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			resource(rs.GetResource())
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					sp.Attributes = apply(sp.GetAttributes())
					events := sp.GetEvents()[:0]
					for _, ev := range sp.GetEvents() {
						if !toolContent && contains(toolContentEvents, ev.GetName()) {
							changed++
							continue
						}
						ev.Attributes = apply(ev.GetAttributes())
						events = append(events, ev)
					}
					sp.Events = events
				}
			}
		}
	}
	return changed
}

func withholdAttrs(attrs []*commonpb.KeyValue, prompts, toolContent bool) ([]*commonpb.KeyValue, bool) {
	marker := claudeRedacted
	for _, kv := range attrs {
		if k := kv.GetKey(); k == "conversation.id" || k == "thread.id" {
			marker = codexRedacted
		}
	}
	changed := false
	out := attrs[:0]
	for _, kv := range attrs {
		switch {
		case !toolContent && contains(toolContentFields, kv.GetKey()):
			changed = true
			continue
		case !prompts && contains(promptDropFields, kv.GetKey()):
			changed = true
			continue
		case !prompts && contains(promptFields, kv.GetKey()) && kv.GetValue().GetStringValue() != marker:
			kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: marker}}
			changed = true
		}
		out = append(out, kv)
	}
	return out, changed
}

func attrString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func contains(set []string, s string) bool {
	return slices.Contains(set, s)
}
