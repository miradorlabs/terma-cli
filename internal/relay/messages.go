package relay

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// roleTool is a tool result's message: every part of it is the tool's output, whatever its type.
const roleTool = "tool"

var (
	// saidParts are the part types of a message envelope that are what was said; every other
	// type (a tool call, its response, a file, a provider's own) is withheld with tool content.
	saidParts = []string{"text", "reasoning"}
	// messageKeys are the keys a message keeps: the content is in its parts.
	messageKeys = []string{"role", "name", "finish_reason", "parts"}
)

// message is one message of an envelope as it leaves: its role, the name of the tool a tool
// message answers (another role's name is a participant's, free text), why the model stopped,
// and the parts that were said.
type message struct {
	Role         string            `json:"role"`
	Name         string            `json:"name,omitempty"`
	FinishReason string            `json:"finish_reason,omitempty"`
	Parts        []json.RawMessage `json:"parts"`
}

// saidPart is a text or reasoning part.
type saidPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// withoutToolParts is a message envelope (shape.CaptureRules.MessageFields) holding only what
// was said: each message's text and reasoning parts, a tool message's none, under its role, so a message left with
// none still marks its turn. It is rebuilt from what was read, so nothing a reader of the JSON
// would not see (a repeated key) passes. did reports whether anything was removed, and ok
// whether v is an envelope at all, each message with a role and parts; when it is not, nothing
// in it can be told apart and it is withheld whole.
func withoutToolParts(v *commonpb.AnyValue) (said string, did, ok bool) {
	raw, isString := v.GetValue().(*commonpb.AnyValue_StringValue)
	if !isString {
		return "", false, false
	}
	var keys []map[string]json.RawMessage
	var msgs []message
	if json.Unmarshal([]byte(raw.StringValue), &keys) != nil || json.Unmarshal([]byte(raw.StringValue), &msgs) != nil {
		return "", false, false
	}
	for i := range msgs {
		// A message with no role cannot be told from a tool's: it is withheld whole.
		if _, has := keys[i]["parts"]; !has || msgs[i].Role == "" {
			return "", false, false
		}
		for key := range keys[i] {
			did = did || !slices.Contains(messageKeys, key)
		}
		tool := strings.EqualFold(msgs[i].Role, roleTool)
		if !tool && msgs[i].Name != "" {
			msgs[i].Name, did = "", true
		}
		kept := make([]json.RawMessage, 0, len(msgs[i].Parts))
		for _, p := range msgs[i].Parts {
			part, ok := textPart(p)
			if !ok || tool {
				did = true
				continue
			}
			b, err := compactJSON(part)
			if err != nil {
				return "", false, false
			}
			kept = append(kept, b)
		}
		msgs[i].Parts = kept
	}
	b, err := compactJSON(msgs)
	if err != nil {
		return "", false, false
	}
	return string(b), did, true
}

// textPart is p when it is a text or reasoning part with nothing in it but its text.
func textPart(p json.RawMessage) (saidPart, bool) {
	var keys map[string]json.RawMessage
	var part saidPart
	if json.Unmarshal(p, &keys) != nil || len(keys) != 2 || keys["type"] == nil || keys["content"] == nil ||
		json.Unmarshal(p, &part) != nil {
		return saidPart{}, false
	}
	return part, slices.Contains(saidParts, part.Type)
}

// compactJSON is v as compact JSON with its text as it came, no HTML escaping.
func compactJSON(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
