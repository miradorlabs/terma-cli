package opencode

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names the plugin's session on every signal.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

// CaptureRules are where the plugin's telemetry carries content: the prompt and the
// session title that restates it ride log bodies, and local paths ride attributes.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptBodyEvents: []string{"opencode.user_prompt", "opencode.session.created"},
		// The session's directory is a local path, as a tool's file path is.
		ToolContentFields: []string{"opencode.tool.file_path", "opencode.session.directory"},
		SafeKeys:          []string{"opencode.version", "opencode.agent", "opencode.message.id", "opencode.parent_message.id", "opencode.project.id"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
