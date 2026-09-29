package relay

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The agents' global exporters send content — prompts, replies, tool arguments and
// output — because the relay cannot know a record's repository before it has placed the
// session. The relay then applies each project's own content policy before anything
// leaves the machine (or reaches the outbox): what a project withholds is removed from
// that project's records only. The field sets are PR #27's (the difference between
// live/golden/<harness>/telemetry-content.json and telemetry-redacted.json, plus the
// places the attribute goldens do not see).

// ContentPolicy is what a project lets its records carry.
type ContentPolicy struct {
	Prompts     bool `json:"include_prompts"`
	ToolContent bool `json:"include_tool_content"`
}

// allowsAll reports whether the policy withholds nothing.
func (p ContentPolicy) allowsAll() bool { return p.Prompts && p.ToolContent }

var (
	// promptFields hold what was said. The harnesses blank them rather than drop them
	// when prompts are off, and the relay does the same, with each harness's own
	// marker, so the backend sees the shape it already parses.
	promptFields = []string{"prompt", "response", "user_prompt"}
	// promptDropFields hold what was said and are removed outright, as the exporters
	// that write them omit them when content is off (the GenAI content attributes;
	// terma's OpenCode plugin writes gen_ai.completion).
	promptDropFields = []string{"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages",
		"gen_ai.output.messages", "gen_ai.system_instructions"}
	// promptBodyEvents carry what was said in the log body: OpenCode's prompt, and the
	// session title, which restates it.
	promptBodyEvents = []string{"opencode.user_prompt", "opencode.session.created"}
	// toolContentFields hold what a tool was called with or returned.
	toolContentFields = []string{"tool_parameters", "tool_input", "full_command", "bash_command", "arguments",
		"output", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "opencode.tool.file_path"}
	// toolContentEvents are span events that exist only to carry tool content: Claude
	// Code's claude_code.tool span records the command and its output (and a file
	// tool's content and diff) as a tool.output event (2.1.284).
	toolContentEvents = []string{"tool.output", "tool.input"}
)

const (
	claudeRedacted = "<REDACTED>"
	codexRedacted  = "[REDACTED]"
)

// withholdRecord applies p to one log record or span, returning the record and whether
// it changed. A record it cannot read is returned unchanged: the gateway judges it.
func withholdRecord(sig signal, raw json.RawMessage, p ContentPolicy) (json.RawMessage, bool) {
	if p.allowsAll() || sig == sigMetrics {
		return raw, false
	}
	var rec object
	if json.Unmarshal(raw, &rec) != nil {
		return raw, false
	}
	changed := false
	attrs, marker, did := withholdAttrs(rec["attributes"], p, "")
	if did {
		rec["attributes"], changed = attrs, true
	}
	if sig == sigLogs && !p.Prompts && slices.Contains(promptBodyEvents, attrValue(rec["attributes"], "event.name")) {
		if body, err := marshal(map[string]string{"stringValue": ""}); err == nil && string(rec["body"]) != string(body) {
			rec["body"], changed = body, true
		}
	}
	if sig == sigTraces {
		var events []object
		if json.Unmarshal(rec["events"], &events) == nil && len(events) > 0 {
			kept := events[:0]
			for _, ev := range events {
				var name string
				_ = json.Unmarshal(ev["name"], &name)
				if !p.ToolContent && slices.Contains(toolContentEvents, name) {
					changed = true
					continue
				}
				if a, _, did := withholdAttrs(ev["attributes"], p, marker); did {
					ev["attributes"], changed = a, true
				}
				kept = append(kept, ev)
			}
			if changed {
				if b, err := marshal(kept); err == nil {
					rec["events"] = b
				}
			}
		}
	}
	if !changed {
		return raw, false
	}
	out, err := marshal(rec)
	if err != nil {
		return raw, false
	}
	return out, true
}

// withholdAttrs applies p to an OTLP attribute list. marker is the redaction marker to
// use; "" picks the harness's own from the list (Codex's records name a conversation or
// thread). It returns the list, the marker it used, and whether anything changed.
func withholdAttrs(raw json.RawMessage, p ContentPolicy, marker string) (json.RawMessage, string, bool) {
	var attrs []object
	if len(raw) == 0 || json.Unmarshal(raw, &attrs) != nil {
		return raw, marker, false
	}
	if marker == "" {
		marker = claudeRedacted
		for _, a := range attrs {
			if k := attrKey(a); k == "conversation.id" || k == "thread.id" {
				marker = codexRedacted
			}
		}
	}
	redacted, _ := marshal(map[string]string{"stringValue": marker})
	changed := false
	kept := attrs[:0]
	for _, a := range attrs {
		k := attrKey(a)
		switch {
		case !p.ToolContent && slices.Contains(toolContentFields, k):
			changed = true
			continue
		case !p.Prompts && slices.Contains(promptDropFields, k):
			changed = true
			continue
		case !p.Prompts && slices.Contains(promptFields, k) && string(a["value"]) != string(redacted):
			a["value"], changed = redacted, true
		}
		kept = append(kept, a)
	}
	if !changed {
		return raw, marker, false
	}
	out, err := marshal(kept)
	if err != nil {
		return raw, marker, false
	}
	return out, marker, true
}

func attrKey(a object) string {
	var k string
	_ = json.Unmarshal(a["key"], &k)
	return k
}

// attrValue is the string value of key in an attribute list, "" when absent.
func attrValue(raw json.RawMessage, key string) string {
	var attrs []kv
	if json.Unmarshal(raw, &attrs) != nil {
		return ""
	}
	for _, a := range attrs {
		if a.Key == key && a.Value.String != nil {
			return *a.Value.String
		}
	}
	return ""
}

// withhold applies p to every log record and span routed to route, returning how many
// it changed. Metric data points carry no content.
func (b *batch) withhold(route string, p ContentPolicy) int {
	if p.allowsAll() || b.sig == sigMetrics {
		return 0
	}
	n := 0
	for ri := range b.resources {
		for si := range b.resources[ri].scopes {
			units := b.resources[ri].scopes[si].units
			for ui := range units {
				u := &units[ui]
				if u.dataKey != "" || len(u.items) != 1 || u.items[0].route != route {
					continue
				}
				if raw, did := withholdRecord(b.sig, u.raw, p); did {
					u.raw = raw
					n++
				}
			}
		}
	}
	return n
}

// policyDir holds each project's content policy, and the machine project's default.
const policyDir = "policy"

// SaveContentPolicy records project's content policy; MachineRoute names the default
// for the machine project and for projects with none of their own. `terma setup` writes
// the default, `terma install` a repository's project.
func SaveContentPolicy(project string, p ContentPolicy) error {
	if !validRoute(project) {
		return errors.New("invalid project id")
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	return config.WriteJSON(filepath.Join(dir, policyDir, project+".json"), p, 0o600)
}

// LoadContentPolicy is project's content policy: its own, else the machine default. A
// machine with neither withholds everything — content leaves only by a recorded choice.
// ok is false when neither is recorded.
func LoadContentPolicy(project string) (ContentPolicy, bool) {
	dir, err := Dir()
	if err != nil {
		return ContentPolicy{}, false
	}
	return loadContentPolicy(dir, project)
}

func loadContentPolicy(dir, project string) (ContentPolicy, bool) {
	for _, name := range []string{project, MachineRoute} {
		if !validRoute(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, policyDir, name+".json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var p ContentPolicy
		if err != nil || json.Unmarshal(data, &p) != nil {
			// Unreadable: fail closed, the file might be the one that withholds.
			return ContentPolicy{}, true
		}
		return p, true
	}
	return ContentPolicy{}, false
}
