package antigravity

import (
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// The workspace rule (design 3.3): a conversation whose workspaces are not all in checkouts the
// policy collects sends no content at all.

// A conversation with a workspace the policy does not collect, resumed from another
// repository or from outside git, sends no content of any kind; its prompt still counts.
func TestAntigravityWorkspaceElsewhereSendsNoContent(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	none, _ := filepath.EvalSymlinks(t.TempDir())
	for name, second := range map[string]func(root string) string{
		"an excluded repository": func(string) string { return excluded },
		"outside git":            func(string) string { return none },
		// A removed clone that was nested in the collected checkout.
		"a folder that does not exist": func(root string) string { return filepath.Join(root, "vendor", "removed") },
	} {
		t.Run(name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.workspaces = []string{g.root, second(g.root)}
			g.turnOne()
			got := g.drain()
			if len(got[semconv.TermaModelCallEvent]) != 3 || len(got[semconv.TermaToolCallEvent]) != 2 || len(got[semconv.TermaUserMessageEvent]) != 1 {
				t.Fatalf("what goes today did not: %v", got)
			}
			if _, text := got[semconv.TermaUserMessageEvent][0].Attrs[semconv.TermaMessageTextKey]; text ||
				len(got[semconv.TermaAssistantMessageEvent]) != 0 || len(got[semconv.TermaToolResultEvent]) != 0 {
				t.Errorf("content left: %v", got)
			}
			for _, c := range got[semconv.TermaToolCallEvent] {
				if _, args := c.Attrs[semconv.GenAIToolCallArgumentsKey]; args {
					t.Errorf("arguments left: %v", c.Attrs)
				}
			}
		})
	}
}
