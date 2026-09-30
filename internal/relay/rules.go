package relay

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// rules are every registered agent's telemetry shape, composed once: one session-key
// precedence, and one content classifier, since a key that is content for any agent is
// content for all.
type rules struct {
	sessionKeys          []shape.SessionKey
	startEvents          []string
	promptFields         []string
	promptDropFields     []string
	promptBodyEvents     []string
	resourcePromptFields []string
	toolContentFields    []string
	toolContentEvents    []string
	markers              []shape.CaptureRules
	bodyPrefixes         []string
	safeKeys             map[string]bool
	safePrefixes         []string
}

// defaultMarker replaces a prompt field that no agent's marker keys claim.
const defaultMarker = "<REDACTED>"

// generic is content no one agent owns: the GenAI semantic conventions' content
// attributes, free text that may restate what was said, and a process's command line.
var generic = shape.CaptureRules{
	PromptDropFields: []string{"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages", "gen_ai.output.messages",
		"gen_ai.system_instructions", "gen_ai.tool.definitions", "gen_ai.tool.description", "gen_ai.agent.description",
		"gen_ai.request.stop_sequences",
		"error", "reason", "reasoning", "routing.reasoning", "metadata", "value", "key", "from", "db", "query_script"},
	ResourcePromptFields: []string{"process.command_args", "process.command_line"},
	ToolContentFields:    []string{"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "file_path", "result"},
}

func compose(correlators []shape.Correlator, capturers []shape.Capturer) *rules {
	r := &rules{}
	byAttr := map[string]shape.SessionKey{}
	for _, c := range correlators {
		cor := c.Correlation()
		for _, k := range cor.SessionKeys {
			if prev, ok := byAttr[k.Attr]; ok && prev != k {
				panic(fmt.Sprintf("relay: session key %q declared as %+v and %+v", k.Attr, prev, k))
			}
			byAttr[k.Attr] = k
		}
		r.startEvents = union(r.startEvents, cor.StartEvents)
	}
	r.sessionKeys = slices.SortedFunc(maps.Values(byAttr), func(a, b shape.SessionKey) int {
		return cmp.Or(cmp.Compare(a.Rank, b.Rank), strings.Compare(a.Attr, b.Attr))
	})
	r.safeKeys, r.safePrefixes = maps.Clone(genericSafeKeys), slices.Clone(genericSafePrefixes)
	all := []shape.CaptureRules{generic}
	for _, c := range capturers {
		all = append(all, c.CaptureRules())
	}
	for _, c := range all {
		r.promptFields = union(r.promptFields, c.PromptFields)
		r.promptDropFields = union(r.promptDropFields, c.PromptDropFields)
		r.promptBodyEvents = union(r.promptBodyEvents, c.PromptBodyEvents)
		r.resourcePromptFields = union(r.resourcePromptFields, c.ResourcePromptFields)
		r.toolContentFields = union(r.toolContentFields, c.ToolContentFields)
		r.toolContentEvents = union(r.toolContentEvents, c.ToolContentEvents)
		r.bodyPrefixes = union(r.bodyPrefixes, c.BodyPrefixes)
		for _, k := range c.SafeKeys {
			r.safeKeys[k] = true
		}
		r.safePrefixes = union(r.safePrefixes, c.SafePrefixes)
		if c.Marker != "" && len(c.MarkerKeys) > 0 {
			r.markers = append(r.markers, shape.CaptureRules{Marker: c.Marker, MarkerKeys: slices.Clone(c.MarkerKeys)})
		}
	}
	return r
}

func union(set, add []string) []string {
	for _, s := range add {
		if !slices.Contains(set, s) {
			set = append(set, s)
		}
	}
	return set
}

// marker is what replaces a prompt field in attrs: the first agent's marker whose keys
// attrs carries, else the default.
func (r *rules) marker(attrs []*commonpb.KeyValue) string {
	for _, m := range r.markers {
		for _, kv := range attrs {
			if slices.Contains(m.MarkerKeys, kv.GetKey()) {
				return m.Marker
			}
		}
	}
	return defaultMarker
}
