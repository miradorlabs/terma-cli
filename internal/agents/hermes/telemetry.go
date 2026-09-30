package hermes

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names the plugin's session on every signal.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

// CaptureRules are where the plugin's telemetry carries content.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{PromptBodyEvents: []string{"hermes.user_prompt", "hermes.assistant_response"}}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
