package runs

import "github.com/miradorlabs/terma-cli/internal/semconv"

// Locational reports whether a record named name says only where something happened (a
// file event, a commit, a push) and carries no content. Only such a record, routed by its
// own location, keeps its destination once its run turns Mixed; delivery and the relay
// drop every other record of a Mixed run, all telemetry included.
//
// None of them carries a prompt, a reply, a tool's input or output, a file's contents, a
// commit message or a diff: only ids, counts, enums, the agent's and tool's names, and
// the location itself, paths and branch names included, which name where the work
// happened even when the developer chose them. A test over the registry holds each of
// their attributes to that (TestLocationalEventsCarryNoConversation).
func Locational(name string) bool {
	switch name {
	case semconv.TermaFilesTouchedEvent, semconv.TermaCommitEvent, semconv.TermaCommitStampedEvent,
		semconv.TermaCommitUnattributedEvent, semconv.TermaPushEvent:
		return true
	}
	return false
}
