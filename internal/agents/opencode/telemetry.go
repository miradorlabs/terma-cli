package opencode

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names the plugin's session on every signal.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

// CaptureRules are where the plugin's telemetry carries content: the prompt and the
// session title that restates it ride log bodies.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptBodyEvents:  []string{"opencode.user_prompt", "opencode.session.created"},
		ToolContentFields: []string{"opencode.tool.file_path"},
		SafeKeys:          []string{"opencode.version", "opencode.agent", "opencode.message.id", "opencode.parent_message.id", "opencode.project.id", "opencode.session.directory"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
