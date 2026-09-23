package hookrun

import "encoding/json"

// jsonUnmarshalInto decodes a toolCall object into the input's ToolCall field.
func jsonUnmarshalInto(raw string, in *antigravityHookInput) error {
	return json.Unmarshal([]byte(`{"conversationId":"c1","toolCall":`+raw+`}`), in)
}
