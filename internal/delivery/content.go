package delivery

import (
	"maps"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// hookContent is where each hook event kind carries what was said or done: the one list
// delivery withholds by. Hooks spool events whole and know no policy; the team's policy
// when an event leaves decides what of it does, as the relay decides for exports. A reply
// or a thread's name is content through and through, so Allowed sends it or not, whole.
var hookContent = map[string]struct{ prompts, toolContent []string }{
	hookrun.EventUserPrompt:    {prompts: []string{"prompt"}},
	hookrun.EventToolCall:      {toolContent: []string{"arguments", "output"}},
	hookrun.EventApprovalAsked: {toolContent: []string{hookrun.AttrReason}},
}

// contentFree are the event kinds that carry no content, or (a reply, a thread's name)
// that Allowed sends or withholds whole. A kind in neither list is withheld whenever the
// policy withholds any content, so a new kind leaks nothing until it is classified.
var contentFree = map[string]bool{
	hookrun.EventSessionStart: true, hookrun.EventSessionEnd: true, hookrun.EventModelCall: true,
	hookrun.EventTurnSummary: true, hookrun.EventCompaction: true, hookrun.EventFilesTouched: true,
	hookrun.EventCommitStamped: true, hookrun.EventCommit: true, hookrun.EventCommitUnattributed: true,
	hookrun.EventSessionQuota: true, hookrun.EventSessionAccount: true, hookrun.EventSessionLimit: true,
	hookrun.EventSessionCapture: true, hookrun.EventSubagentStart: true, hookrun.EventSubagentEnd: true,
	hookrun.EventSubagentCall: true, hookrun.EventSessionObservation: true,
	hookrun.EventAssistantMessage: true, hookrun.EventSessionTitle: true,
}

// Outgoing is e as it may leave for projectID under org, the team's policy now, without
// the content that policy withholds (config.Policy.Content, the relay's rule); false when
// none of it may leave.
func (r Router) Outgoing(org config.Policy, projectID string, e spool.Event) (spool.Event, bool) {
	if !r.Allowed(org, projectID, e) {
		return spool.Event{}, false
	}
	prompts, toolContent := routing.EffectivePolicy(org, projectID).Content()
	c, ok := hookContent[e.Name]
	if !ok {
		return e, contentFree[e.Name] || prompts && toolContent
	}
	var drop []string
	if !prompts {
		drop = append(drop, c.prompts...)
	}
	if !toolContent {
		drop = append(drop, c.toolContent...)
	}
	if len(drop) > 0 {
		e.Attrs = maps.Clone(e.Attrs)
		for _, k := range drop {
			delete(e.Attrs, k)
		}
	}
	return e, true
}
