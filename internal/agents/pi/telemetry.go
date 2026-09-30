package pi

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names the extension's session on every signal.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

// CaptureRules are where the extension's telemetry carries content.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{PromptBodyEvents: []string{"pi.user_prompt"}}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
