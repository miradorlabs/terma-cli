package omp

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names omp's session: session.id on the extension's logs,
// gen_ai.conversation.id on its spans.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID, shape.GenAIConversationID}}
}

// CaptureRules are where omp's telemetry carries content.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptDropFields: []string{"omp.gen_ai.request.messages", "omp.gen_ai.response.text"},
		PromptBodyEvents: []string{"omp.user_prompt"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
