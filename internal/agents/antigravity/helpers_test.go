package antigravity

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
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
