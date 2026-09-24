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

		AttrProjectID:      "project_id",
		attrAgentParentID:  "agent_parent_id",
		attrTool:           "tool",
		attrModel:          "model",
		attrVersion:        "terma.version",
		attrSource:         "source",
		attrReason:         "reason",
		attrStatus:         "status",
		attrTurnID:         "turn_id",
		attrToolName:       "tool_name",
		attrToolCallID:     "tool_call_id",
		attrAgentID:        "agent_id",
		attrAgentType:      "agent_type",
		attrParentSession:  "parent_session_id",
		attrFileCount:      "file_count",
		attrAccountID:      "account_id",
		attrSchemaVersion:  "schema_version",
		attrEvidenceSource: "evidence_source",
		attrEvidenceStatus: "evidence_status",
		attrHookEvent:      "hook_event",

		sourceCursorHook:      "cursor_hook",
		sourceAntigravityHook: "antigravity_hook",
		sourceCodexRollout:    "codex_rollout",
		statusPresent:         "present",
		statusUnavailable:     "unavailable",
		unknownValue:          "unknown",

		claudeTool:      "claude-code",
		codexTool:       "codex",
		opencodeTool:    "opencode",
		cursorTool:      "cursor",
		antigravityTool: "antigravity",
	} {
		if got != want {
			t.Errorf("wire name %q changed; it must stay %q", got, want)
		}
	}
}
