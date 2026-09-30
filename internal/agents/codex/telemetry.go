package codex

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names a Codex thread: conversation.id on logs, thread.id on the turn span,
// thread_id on session_loop. A numeric thread id is an OS thread, never a session.
// app-server exports conversation_starts at thread/start, long before the first hook.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{
		SessionKeys: []shape.SessionKey{
			{Attr: "conversation.id", Rank: 20},
			{Attr: "thread.id", Rank: 40, RejectNumeric: true},
			{Attr: "thread_id", Rank: 50, RejectNumeric: true},
		},
		StartEvents: []string{"codex.conversation_starts"},
	}
}

// CaptureRules are where Codex's telemetry carries content, blanked with Codex's own
// marker so the backend sees the shape it already parses.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptFields:      []string{"prompt"},
		ToolContentFields: []string{"arguments", "output"},
		Marker:            "[REDACTED]",
		MarkerKeys:        []string{"conversation.id", "thread.id"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
