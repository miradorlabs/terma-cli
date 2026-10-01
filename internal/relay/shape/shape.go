// Package shape is what a coding agent declares about its telemetry for the local relay:
// how its records name their session (Correlator) and where they carry content (Capturer).
// Agents declare; the relay enforces, and pins the composed safe keys in its tests.
package shape

// SessionKey is an attribute that names the session a record belongs to.
type SessionKey struct {
	Attr string
	// Rank orders keys across agents, lowest first, so registration order never changes routing.
	Rank int
	// RejectNumeric: a numeric value is never a session (an OS thread id).
	RejectNumeric bool
}

// The session keys several agents share, ranked ahead of and after any one agent's.
var (
	SessionID           = SessionKey{Attr: "session.id", Rank: 10}
	GenAIConversationID = SessionKey{Attr: "gen_ai.conversation.id", Rank: 30}
)

// Correlation is how an agent's telemetry is placed in a session.
type Correlation struct {
	SessionKeys []SessionKey
	// StartEvents may arrive long before a hook claims their session, so they wait as long as a trace.
	StartEvents []string
}

// Correlator is an agent whose telemetry the relay places by session.
type Correlator interface {
	Correlation() Correlation
}

// CaptureRules are where an agent's telemetry carries what was said or a tool's input and output.
type CaptureRules struct {
	// PromptFields are blanked to the marker when prompts are withheld.
	PromptFields []string
	// PromptDropFields, PromptBodyEvents and ResourcePromptFields are removed when prompts are withheld.
	PromptDropFields     []string
	PromptBodyEvents     []string
	ResourcePromptFields []string
	// ToolContentFields and ToolContentEvents are removed when tool content is withheld.
	ToolContentFields []string
	ToolContentEvents []string
	// Marker replaces a prompt field in any attribute set carrying one of MarkerKeys.
	Marker       string
	MarkerKeys   []string
	BodyPrefixes []string
	// SafeKeys and SafePrefixes pass with content withheld, unless another agent declares them content.
	SafeKeys     []string
	SafePrefixes []string
}

// Capturer is an agent whose telemetry the relay withholds content from.
type Capturer interface {
	CaptureRules() CaptureRules
}
