package relay

// The relay's content and session rules as they were before each agent declared its
// own (internal/relay/shape): the reference the composed rules are held to.

import (
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

var (
	// legacyPromptFields hold what was said. The harnesses blank them rather than drop
	// them when prompts are off, and the relay does the same, with each harness's own
	// marker, so the backend sees the shape it already parses.
	legacyPromptFields = []string{"prompt", "response", "user_prompt"}
	// legacyPromptDropFields hold what was said and are removed outright, as the exporters
	// that write them omit them when content is off: the GenAI semantic conventions'
	// content attributes (terma's OpenCode plugin writes gen_ai.completion).
	legacyPromptDropFields = []string{"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
		"gen_ai.tool.definitions",
		// omp's own (omp.gen_ai.*): the request's messages and the response's text.
		"omp.gen_ai.request.messages", "omp.gen_ai.response.text",
		// Gemini CLI's gemini_cli.api_request / api_response (0.62).
		"request_text", "response_text",
		// Free text that may restate or reason about what was said, in the harnesses'
		// own events (the first unclassified-key survey, 2026-09-30): errors, reasons,
		// Gemini's model-router reasoning, tool and agent descriptions, stop sequences,
		// and keys too generic to vouch for.
		"error", "reason", "reasoning", "routing.reasoning", "metadata", "value", "key", "from", "db", "query_script",
		"gen_ai.tool.description", "gen_ai.agent.description", "gen_ai.request.stop_sequences"}
	// legacyResourcePromptFields are resource attributes that restate what was said: the
	// process's command line, which carries a prompt passed as an argument (Gemini CLI
	// stamps process.command_args, `-p "<prompt>"` included, whatever logPrompts says).
	legacyResourcePromptFields = []string{"process.command_args", "process.command_line"}
	// legacyPromptBodyEvents carry what was said in the log body, not an attribute: the
	// OpenCode plugin's prompt, the session title, which restates it, Pi's prompt, and
	// Hermes's prompt and reply.
	legacyPromptBodyEvents = []string{"opencode.user_prompt", "opencode.session.created", "pi.user_prompt", "omp.user_prompt", "hermes.user_prompt", "hermes.assistant_response", "dsh.user_prompt", "dsh.assistant_response"}
	// legacyToolContentFields hold what a tool was called with or returned.
	legacyToolContentFields = []string{"tool_parameters", "tool_input", "full_command", "bash_command", "arguments", "output",
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "opencode.tool.file_path",
		// Gemini CLI's tool_call and hook_call records (0.62).
		"function_args", "hook_input", "hook_output", "stdout", "stderr",
		// What a tool acted on or returned, as named elsewhere.
		"file_path", "result"}
	// legacyToolContentEvents are span events that exist only to carry tool content: Claude
	// Code's claude_code.tool span records the command and its output as a
	// tool.output event (live, 2.1.284), which the golden attribute lists do not see.
	legacyToolContentEvents = []string{"tool.output", "tool.input"}
)

const (
	legacyClaudeRedacted = "<REDACTED>"
	legacyCodexRedacted  = "[REDACTED]"
)

func legacyWithhold(p *part, prompts, toolContent bool, unclassified map[string]int) int {
	if prompts && toolContent {
		return 0
	}
	changed := 0
	apply := func(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
		out, did := legacyWithholdAttrs(attrs, prompts, toolContent, unclassified)
		if did {
			changed++
		}
		return out
	}
	resource := func(r *resourcepb.Resource) {
		if r == nil {
			return
		}
		kept := r.Attributes[:0]
		for _, kv := range r.GetAttributes() {
			switch key := kv.GetKey(); {
			case contains(legacyResourcePromptFields, key):
				if !prompts {
					changed++
					continue
				}
			case !safeKey(key):
				unclassified["resource/"+key]++
				changed++
				continue
			}
			kept = append(kept, kv)
		}
		r.Attributes = kept
	}
	exemplars := func(values []*metricspb.Exemplar) {
		for _, value := range values {
			value.FilteredAttributes = apply(value.FilteredAttributes)
		}
	}
	switch m := p.msg.(type) {
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			resource(rm.GetResource())
			for _, sm := range rm.GetScopeMetrics() {
				if sm.Scope != nil {
					sm.Scope.Attributes = apply(sm.Scope.Attributes)
				}
				for _, mt := range sm.GetMetrics() {
					for _, pt := range mt.GetSum().GetDataPoints() {
						pt.Attributes = apply(pt.GetAttributes())
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetGauge().GetDataPoints() {
						pt.Attributes = apply(pt.GetAttributes())
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetHistogram().GetDataPoints() {
						pt.Attributes = apply(pt.GetAttributes())
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetExponentialHistogram().GetDataPoints() {
						pt.Attributes = apply(pt.Attributes)
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetSummary().GetDataPoints() {
						pt.Attributes = apply(pt.Attributes)
					}
				}
			}
		}
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			resource(rl.GetResource())
			for _, sl := range rl.GetScopeLogs() {
				if sl.Scope != nil {
					sl.Scope.Attributes = apply(sl.Scope.Attributes)
				}
				for _, lr := range sl.GetLogRecords() {
					lr.Attributes = apply(lr.GetAttributes())
					event := attrString(lr.GetAttributes(), "event.name")
					_, plain := lr.GetBody().GetValue().(*commonpb.AnyValue_StringValue)
					body := lr.GetBody().GetStringValue()
					switch {
					case lr.GetBody().GetValue() == nil || plain && body == "":
					case !prompts && contains(legacyPromptBodyEvents, event):
						lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ""}}
						changed++
					case !plain || !legacyBodyNamesItsEvent(body, event):
						// A body is free text: kept only when it just names its event.
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
				if ss.Scope != nil {
					ss.Scope.Attributes = apply(ss.Scope.Attributes)
				}
				for _, sp := range ss.GetSpans() {
					// Provider errors can repeat either prompts or tool input/output.
					if sp.Status != nil && sp.Status.Message != "" {
						sp.Status.Message = ""
						changed++
					}
					for _, link := range sp.Links {
						link.Attributes = apply(link.Attributes)
					}
					sp.Attributes = apply(sp.GetAttributes())
					events := sp.GetEvents()[:0]
					for _, ev := range sp.GetEvents() {
						if !toolContent && contains(legacyToolContentEvents, ev.GetName()) {
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

func legacyWithholdAttrs(attrs []*commonpb.KeyValue, prompts, toolContent bool, unclassified map[string]int) ([]*commonpb.KeyValue, bool) {
	marker := legacyClaudeRedacted
	for _, kv := range attrs {
		if k := kv.GetKey(); k == "conversation.id" || k == "thread.id" {
			marker = legacyCodexRedacted
		}
	}
	changed := false
	out := attrs[:0]
	for _, kv := range attrs {
		key := kv.GetKey()
		switch {
		case !toolContent && contains(legacyToolContentFields, key):
			changed = true
			continue
		case !prompts && contains(legacyPromptDropFields, key):
			changed = true
			continue
		case !prompts && contains(legacyPromptFields, key) && kv.GetValue().GetStringValue() != marker:
			kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: marker}}
			changed = true
		case !legacyContentKey(key) && !safeKey(key) && !scalarNonText(kv.GetValue()):
			// Withheld content passes only what is known to be safe (allow.go).
			unclassified[key]++
			changed = true
			continue
		}
		out = append(out, kv)
	}
	return out, changed
}

func legacyBodyNamesItsEvent(body, event string) bool {
	return body == event || body == "claude_code."+event || event != "" && strings.HasSuffix(body, "."+event)
}

var legacySessionKeys = []string{"session.id", "conversation.id", "gen_ai.conversation.id", "thread.id", "thread_id"}

func legacySessionOf(attrs, resource []*commonpb.KeyValue) string {
	for _, set := range [][]*commonpb.KeyValue{attrs, resource} {
		for _, key := range legacySessionKeys {
			for _, kv := range set {
				if kv.GetKey() == key {
					if v := kv.GetValue().GetStringValue(); v != "" && (key != "thread.id" && key != "thread_id" || !numeric(v)) {
						return v
					}
				}
			}
		}
	}
	return ""
}

func legacyConversationStart(p *part) bool {
	m, ok := p.msg.(*logspb.LogsData)
	if !ok {
		return false
	}
	for _, rl := range m.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				for _, kv := range lr.GetAttributes() {
					if kv.GetKey() == "event.name" && kv.GetValue().GetStringValue() == "codex.conversation_starts" {
						return true
					}
				}
			}
		}
	}
	return false
}

func legacyContentKey(key string) bool {
	return contains(legacyPromptFields, key) || contains(legacyPromptDropFields, key) || contains(legacyToolContentFields, key) ||
		contains(legacyResourcePromptFields, key)
}
