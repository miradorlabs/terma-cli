package hookrun

import "testing"

// The platform's adapters parse these strings. The constants exist so the Go code says
// one name once; this pins what they hold, so tidying a constant cannot rename a field
// on the wire.
func TestWireNamesAreFrozen(t *testing.T) {
	for got, want := range map[string]string{
		EventSessionStart:       "terma.session.start",
		EventUserPrompt:         "terma.user.prompt",
		EventModelCall:          "terma.model.call",
		EventTurnSummary:        "terma.turn.summary",
		EventCompaction:         "terma.compaction",
		EventApprovalAsked:      "terma.approval.requested",
		EventSessionEnd:         "terma.session.end",
		EventFilesTouched:       "terma.files.touched",
		EventCommitStamped:      "terma.commit.stamped",
		EventCommit:             "terma.commit",
		EventCommitUnattributed: "terma.commit.unattributed",
		EventSessionQuota:       "terma.session.quota",
		EventSessionAccount:     "terma.session.account",
		EventSessionLimit:       "terma.session.limit",
		EventSessionCapture:     "terma.session.capture",
		EventSessionObservation: "terma.session.observation",
		EventSubagentStart:      "terma.subagent.start",
		EventSubagentEnd:        "terma.subagent.end",
		EventSubagentCall:       "terma.subagent.call",
		EventToolCall:           "terma.tool.call",
		EventAssistantMessage:   "terma.assistant.message",
		EventSessionTitle:       "terma.session.title",

		AttrProjectID:      "project_id",
		AttrWorktree:       "worktree",
		AttrAgentParentID:  "agent_parent_id",
		AttrTool:           "tool",
		AttrModel:          "model",
		AttrVersion:        "terma.version",
		AttrSource:         "source",
		AttrReason:         "reason",
		AttrStatus:         "status",
		AttrTurnID:         "turn_id",
		AttrToolName:       "tool_name",
		AttrToolCallID:     "tool_call_id",
		AttrAgentID:        "agent_id",
		AttrAgentType:      "agent_type",
		AttrParentSession:  "parent_session_id",
		AttrFileCount:      "file_count",
		AttrAccountID:      "account_id",
		AttrSchemaVersion:  "schema_version",
		AttrEvidenceSource: "evidence_source",
		AttrEvidenceStatus: "evidence_status",
		AttrHookEvent:      "hook_event",

		StatusPresent:     "present",
		StatusUnavailable: "unavailable",
		UnknownValue:      "unknown",
	} {
		if got != want {
			t.Errorf("wire name %q changed; it must stay %q", got, want)
		}
	}
}
