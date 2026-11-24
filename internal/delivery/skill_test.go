package delivery

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A tool call that names the skill it loaded leaves under every policy, with the name only
// while the policy collects prompts and tool content and its agent still has the developer's
// consent.
func TestOutgoingSendsAToolCallsSkillNameOnlyWithAllContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ev := spool.Event{Repository: app, Name: semconv.TermaToolCallEvent, SessionID: "s1", Attrs: map[string]any{
		hookrun.AttrProjectID: "p1", semconv.GenAIMainAgentNameKey: "fake", semconv.GenAIToolNameKey: "read_file",
		semconv.GenAIToolCallIDKey: "step-2", semconv.TermaSkillNameKey: "deploy",
	}}
	consent := func(ok bool) func(string, hookrun.Consent) bool {
		return func(tool string, _ hookrun.Consent) bool { return ok && tool == "fake" }
	}
	policy := func(prompts, toolContent bool) config.Policy {
		return config.Policy{Mode: config.ModeRepo, Repositories: listed, IncludePrompts: prompts, IncludeToolContent: toolContent}
	}
	for _, tc := range []struct {
		name    string
		pol     config.Policy
		consent func(string, hookrun.Consent) bool
		named   bool
	}{
		{"nothing but logs, consented", policy(false, false), consent(true), false},
		{"prompts only, consented", policy(true, false), consent(true), false},
		{"tool content only, consented", policy(false, true), consent(true), false},
		{"everything, consented", policy(true, true), consent(true), true},
		{"everything, deselected", policy(true, true), consent(false), false},
		{"everything, no one to ask", policy(true, true), nil, false},
	} {
		out, ok := (Router{ConfigDir: dir, Consent: tc.consent}).Outgoing(tc.pol, "p1", ev)
		_, named := out.Attrs[semconv.TermaSkillNameKey]
		if !ok || named != tc.named || out.Attrs[semconv.GenAIToolCallIDKey] != "step-2" {
			t.Errorf("%s: %+v, %v", tc.name, out.Attrs, ok)
		}
	}
	if ev.Attrs[semconv.TermaSkillNameKey] != "deploy" {
		t.Error("the queued event was changed")
	}
}
