package runs

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Only a file event, a commit or a push keeps its destination once its run turns Mixed:
// every record that carries content obeys the run, wherever it routes.
func TestOnlyPathsAndGitFactsAreExemptFromMixed(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		semconv.TermaFilesTouchedEvent:       true,
		semconv.TermaCommitEvent:             true,
		semconv.TermaCommitStampedEvent:      true,
		semconv.TermaCommitUnattributedEvent: true,
		semconv.TermaPushEvent:               true,
		semconv.TermaAssistantMessageEvent:   false,
		semconv.TermaSessionTitleEvent:       false,
		semconv.TermaToolCallEvent:           false,
		semconv.TermaSessionQuotaEvent:       false,
		semconv.TermaSessionStartEvent:       false,
		"":                                   false,
	} {
		if got := Locational(name); got != want {
			t.Errorf("Locational(%q) = %v, want %v", name, got, want)
		}
	}
}
