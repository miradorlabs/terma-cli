package hookrun

// Every string in this file is a contract the platform parses byte for byte: add a
// constant, never change what one holds.

// Event names written to the spool.
const (
	EventSessionStart  = "terma.session.start"
	EventUserPrompt    = "terma.user.prompt"
	EventModelCall     = "terma.model.call"
	EventTurnSummary   = "terma.turn.summary"
	EventCompaction    = "terma.compaction"
	EventApprovalAsked = "terma.approval.requested"
	EventSessionEnd    = "terma.session.end"
	EventFilesTouched  = "terma.files.touched"
	EventCommitStamped = "terma.commit.stamped"
	EventCommit        = "terma.commit"
	// EventCommitUnattributed is the count-only record of a commit no session was stamped into.
	EventCommitUnattributed = "terma.commit.unattributed"
	// EventSessionQuota is the provider's rate-limit windows and the session's running estimate.
	EventSessionQuota = "terma.session.quota"
	// EventSessionAccount is a session's account state; EventSessionLimit a typed limit failure.
	EventSessionAccount = "terma.session.account"
	EventSessionLimit   = "terma.session.limit"
	// EventSessionCapture is how a capture is going, kept apart from what it captured.
	EventSessionCapture = "terma.session.capture"
	// EventSubagentStart and EventSubagentEnd bracket a subagent running inside its parent's session.
	EventSubagentStart = "terma.subagent.start"
	EventSubagentEnd   = "terma.subagent.end"
	// EventSubagentCall is the parent's view of the call that launched a subagent; it arrives
	// after the end, from another hook process, and says nothing of spend.
	EventSubagentCall = "terma.subagent.call"
)

// EventSessionObservation is one hook-derived snapshot of a session's state, never an additive counter.
const EventSessionObservation = "terma.session.observation"

// EventToolCall is one tool call from a hooks-only agent, keyed on its own call id, which
// is the replay identity, so it bypasses the observation checkpoint.
const EventToolCall = "terma.tool.call"

// EventAssistantMessage is one reply from an agent whose export leaves its replies out,
// sent only under the consent that governs prompts.
const EventAssistantMessage = "terma.assistant.message"

// EventSessionTitle is the name an agent gave a session and keeps only on disk; a rename
// is a new event, under the consent a reply needs.
const EventSessionTitle = "terma.session.title"

// AttrProjectID is the event attribute carrying the developer's team.
const AttrProjectID = "project_id"

// AttrWorktree is git's name for the linked worktree an event came from; absent in a main checkout.
const AttrWorktree = "worktree"

// Attribute keys more than one agent writes; a key only one event carries stays a literal.
const (
	AttrTool           = "tool"
	AttrModel          = "model"
	AttrVersion        = "terma.version"
	AttrSource         = "source"
	AttrReason         = "reason"
	AttrStatus         = "status"
	AttrTurnID         = "turn_id"
	AttrToolName       = "tool_name"
	AttrToolCallID     = "tool_call_id"
	AttrAgentID        = "agent_id"
	AttrAgentType      = "agent_type"
	AttrAgentParentID  = "agent_parent_id"
	AttrParentSession  = "parent_session_id"
	AttrFileCount      = "file_count"
	AttrAccountID      = "account_id"
	AttrOrganizationID = "organization_id"
	AttrSchemaVersion  = "schema_version"
	AttrEvidenceSource = "evidence_source"
	AttrEvidenceStatus = "evidence_status"
	AttrHookEvent      = "hook_event"
)

// Values of evidence_status and the *_status attributes; the harness package adds "missing" and "unreadable".
const (
	StatusPresent     = "present"
	StatusUnavailable = "unavailable"
)

// UnknownValue replaces an agent's word outside the vocabulary terma forwards, so it never arrives as free text.
const UnknownValue = "unknown"
