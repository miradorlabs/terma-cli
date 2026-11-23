package runs

import "github.com/miradorlabs/terma-cli/internal/semconv"

// Locational reports whether name is a location-only record carrying no conversation or content.
// Only Locational records keep their own checkout's destination after a run turns Mixed.
func Locational(name string) bool {
	return name == semconv.TermaFilesTouchedEvent || name == semconv.TermaCommitEvent ||
		name == semconv.TermaCommitStampedEvent || name == semconv.TermaCommitUnattributedEvent || name == semconv.TermaPushEvent
}
