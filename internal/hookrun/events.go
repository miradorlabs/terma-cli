package hookrun

// Every name in this file is a contract: the platform's terma-cli adapters parse the
// event names, the attribute keys and the values byte for byte. A constant may be added
// or renamed in Go; the string it holds may not change.

// Event names written to the spool. The backend groups on these.
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
	// EventCommitUnattributed is the count-only record of a commit no session was
	// stamped into — the denominator of coverage. See emitUnattributedCommit for
	// its shape and the boundary on what it may carry.
	EventCommitUnattributed = "terma.commit.unattributed"
	// EventSessionQuota is the status line's report of the provider's own rate-limit
	// windows, fast mode and the session's running estimate. See statusline.go.
	EventSessionQuota = "terma.session.quota"
	// EventSessionAccount is the account state and credential-presence hints behind a
	// Claude Code session; EventSessionLimit is the typed failure a StopFailure reports.
	EventSessionAccount = "terma.session.account"
	EventSessionLimit   = "terma.session.limit"
	// EventSessionCapture is how a capture is going, kept apart from what it captured: a
	// rollout backlog or a checkpoint lock that timed out is not a new quota reading.
	EventSessionCapture = "terma.session.capture"
	// EventSubagentStart and EventSubagentEnd bracket a subagent that runs inside its
	// parent's session. See subagent.go for the two shapes a subagent comes in.
	EventSubagentStart = "terma.subagent.start"
	EventSubagentEnd   = "terma.subagent.end"
	// EventSubagentCall is the call that launched a subagent, as its parent saw it
	// return: which tool call it was, the model the subagent resolved to — which no
	// other hook names — and, when the parent waited for it, how the run went: its
	// duration, its tool use, the size its context reached. Not what it spent; no hook
	// says that. It is keyed on agent_id like the other two and arrives
	// after the end it describes, from a separate hook process, which is why it is an
	// event of its own rather than more attributes on terma.subagent.end.
	EventSubagentCall = "terma.subagent.call"
)

// EventSessionObservation is one hook-derived snapshot of a session's state — a turn
// boundary, a model, a token snapshot when the harness supplies one. Observations are
// ordered per conversation and repository and never additive counters.
const EventSessionObservation = "terma.session.observation"

// EventToolCall is one tool invocation a coding agent made, reported by a harness whose
// only signal is terma's hooks. It is per-call evidence keyed on the harness's own call
// id, not a snapshot of the session, which is why it bypasses the observation
// checkpoint: `tool_call_id` is the replay identity, and the spool's at-least-once
// delivery is deduplicated on it downstream. Harnesses with a native OTel export report
// their tool calls there, never here.
const EventToolCall = "terma.tool.call"

// EventAssistantMessage is one thing a coding agent said, for an agent whose own export
// leaves it out. Today that is Codex alone: its OTel events carry the developer's prompts
// and its tools' input and output, and no reply, so a Codex session reads as a person
// talking to tools. The text comes from the rollout (harness.ReadCodexReplies).
//
// It is the single exception to "terma's hooks never read what was said", and it is
// bounded by the consent that already governs content: see codexRepliesConsented.
const EventAssistantMessage = "terma.assistant.message"

// AttrProjectID is the event attribute carrying the project binding.
const AttrProjectID = "project_id"

// Attribute keys more than one adapter writes. A key only one event carries stays a
// literal beside the code that explains it.
const (
	attrTool           = "tool"
	attrModel          = "model"
	attrVersion        = "terma.version"
	attrSource         = "source"
	attrReason         = "reason"
	attrStatus         = "status"
	attrTurnID         = "turn_id"
	attrToolName       = "tool_name"
	attrToolCallID     = "tool_call_id"
	attrAgentID        = "agent_id"
	attrAgentType      = "agent_type"
	attrAgentParentID  = "agent_parent_id"
	attrParentSession  = "parent_session_id"
	attrFileCount      = "file_count"
	attrAccountID      = "account_id"
	attrSchemaVersion  = "schema_version"
	attrEvidenceSource = "evidence_source"
	attrEvidenceStatus = "evidence_status"
	attrHookEvent      = "hook_event"
)

// Values of evidence_source: where a record's facts were read from.
const (
	sourceCursorHook      = "cursor_hook"
	sourceAntigravityHook = "antigravity_hook"
	sourceCodexRollout    = "codex_rollout"
	sourceCodexHook       = "codex_hook"
)

// Values of evidence_status and of the per-facet *_status attributes. The harness
// package reports two more, "missing" and "unreadable", which pass through untouched.
const (
	statusPresent     = "present"
	statusUnavailable = "unavailable"
)

// unknownValue replaces a word from a harness that is outside the vocabulary terma
// forwards, so a new upstream value arrives as a known one instead of as free text.
const unknownValue = "unknown"
