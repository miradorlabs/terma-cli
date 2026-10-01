package antigravity

import "encoding/json"

func jsonUnmarshalInto(raw string, in *antigravityHookInput) error {
	return json.Unmarshal([]byte(`{"conversationId":"c1","toolCall":`+raw+`}`), in)
}
