package delivery

import (
	"maps"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// hookContent is where each hook event kind carries a tool's input: the one list delivery
// withholds by. Hooks spool events whole and know no policy; the team's policy when an
// event leaves decides what of it does, as the relay decides for exports. A reply or a
// thread's name is what was said through and through, so Allowed sends it or not, whole.
var hookContent = map[string][]string{
	semconv.TermaToolCallEvent:          {semconv.GenAIToolCallArgumentsKey},
	semconv.TermaApprovalRequestedEvent: {semconv.TermaApprovalReasonKey},
}

// contentFree are the event kinds that carry no content but terma.repository.root, which
// Outgoing withholds along with tool content, or (a reply, a thread's name)
// that Allowed sends or withholds whole. A kind in neither list is withheld whenever the
// policy withholds any content, so a new kind leaks nothing until it is classified.
var contentFree = map[string]bool{
	semconv.TermaSessionStartEvent: true, semconv.TermaSessionEndEvent: true,
	semconv.TermaCompactionEvent: true, semconv.TermaFilesTouchedEvent: true,
	semconv.TermaCommitStampedEvent: true, semconv.TermaCommitEvent: true, semconv.TermaCommitUnattributedEvent: true,
	semconv.TermaSessionQuotaEvent: true, semconv.TermaSessionAccountEvent: true, semconv.TermaSessionLimitEvent: true,
	semconv.TermaSessionCaptureEvent: true, semconv.TermaSubagentStartEvent: true, semconv.TermaSubagentEndEvent: true,
	semconv.TermaSubagentCallEvent: true, semconv.TermaSessionObservationEvent: true,
	semconv.TermaAssistantMessageEvent: true, semconv.TermaSessionTitleEvent: true,
}

// Outgoing is e as it may leave for projectID under org, the team's policy now, without
// the content that policy withholds (config.Policy.Content, the relay's rule); false when
// none of it may leave. A policy withholding either kind of content withholds user.email
// too, as the relay does; an agent that keys a seat on it sends terma.account.seat.id beside it.
func (r Router) Outgoing(org config.Policy, projectID string, e spool.Event) (spool.Event, bool) {
	if !r.Allowed(org, projectID, e) {
		return spool.Event{}, false
	}
	prompts, toolContent := routing.EffectivePolicy(r.StateDir, org, projectID).Content()
	if prompts && toolContent {
		return e, true
	}
	toolKeys, ok := hookContent[e.Name]
	if !ok && !contentFree[e.Name] {
		return spool.Event{}, false
	}
	e.Attrs = maps.Clone(e.Attrs)
	delete(e.Attrs, semconv.UserEmailKey)
	if !toolContent {
		for _, k := range toolKeys {
			delete(e.Attrs, k)
		}
		// A local path, on whichever event names its checkout: withheld as the relay withholds it.
		delete(e.Attrs, semconv.TermaRepositoryRootKey)
	}
	return e, true
}

// wire is a batch as it is sent: the project leaves as the resource's mirador.project.id,
// so the routing key each event is queued under stays behind.
func wire(batch []spool.Event) []spool.Event {
	out := make([]spool.Event, len(batch))
	for i, e := range batch {
		e.Attrs = maps.Clone(e.Attrs)
		delete(e.Attrs, hookrun.AttrProjectID)
		out[i] = e
	}
	return out
}
