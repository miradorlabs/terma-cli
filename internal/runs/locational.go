package runs

import "github.com/miradorlabs/terma-cli/internal/semconv"

// Locational reports whether a record named name says only where something happened,
// paths or git facts, and carries no conversation content: the files a tool call touched,
// a commit or a push. Only such a record, when it routes by a location of its own (a file
// event's checkout, a commit's repository), keeps its destination once its run turns
// Mixed. Every record that carries content obeys its run's Mixed state wherever it
// routes: a reply, a title, a prompt, tool activity, any turn or conversation record, and
// all telemetry. Delivery and the relay both drop by this rule.
func Locational(name string) bool {
	switch name {
	case semconv.TermaFilesTouchedEvent, semconv.TermaCommitEvent, semconv.TermaCommitStampedEvent,
		semconv.TermaCommitUnattributedEvent, semconv.TermaPushEvent:
		return true
	}
	return false
}
