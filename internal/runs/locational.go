package runs

import "github.com/miradorlabs/terma-cli/internal/semconv"

// Locational reports whether name is a location-only record (file event, commit, push) that
// carries no conversation or content (enforced by TestLocationalEventsCarryNoConversation).
// Only Locational records keep their own checkout's destination after a run turns Mixed.
func Locational(name string) bool {
	switch name {
	case semconv.TermaFilesTouchedEvent, semconv.TermaCommitEvent, semconv.TermaCommitStampedEvent,
		semconv.TermaCommitUnattributedEvent, semconv.TermaPushEvent:
		return true
	}
	return false
}
