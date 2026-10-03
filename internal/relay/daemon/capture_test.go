package daemon

import (
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func TestCapturePolicy(t *testing.T) {
	open := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true}
	global := open
	global.Mode = config.ModeGlobal
	chosen := []string{"claude", "pi"}
	teamOff := config.Policy{Mode: config.ModeRepo, IncludeToolContent: true}
	type want struct {
		prompts, tools, requireClaim bool
		signals                      []string
	}
	for _, test := range []struct {
		name string
		in   Capture
		want want
	}{
		{"a chosen agent sends every signal under the team's policy", Capture{Org: open, Agents: chosen, Harness: "claude"},
			want{true, true, true, nil}},
		{"the team's policy withholds content", Capture{Org: teamOff, Agents: chosen, Harness: "claude"},
			want{false, true, true, nil}},
		{"an agent the developer did not choose is withheld", Capture{Org: open, Agents: chosen, Harness: "codex"},
			want{false, false, true, []string{}}},
		{"a policy that collects nothing withholds everything", Capture{Org: config.NoPolicy("", ""), Agents: chosen, Harness: "claude"},
			want{false, false, true, []string{}}},
		{"catch-all has no agent to check", Capture{Org: global, Primary: true},
			want{true, true, false, nil}},
		{"global mode sends every agent's sessions", Capture{Org: global, Primary: true, Harness: "codex"},
			want{true, true, false, nil}},
		{"another project in global mode needs a claim", Capture{Org: global, Agents: chosen, Harness: "claude"},
			want{true, true, true, nil}},
		{"a primary project out of global mode needs one", Capture{Org: open, Agents: chosen, Primary: true, Harness: "claude"},
			want{true, true, true, nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := CapturePolicy(test.in)
			if got.IncludePrompts != test.want.prompts || got.IncludeToolContent != test.want.tools || got.RequireClaim != test.want.requireClaim ||
				!slices.Equal(got.Signals, test.want.signals) || (got.Signals == nil) != (test.want.signals == nil) {
				t.Fatalf("got prompts=%v tools=%v requireClaim=%v signals=%#v, want %+v", got.IncludePrompts, got.IncludeToolContent, got.RequireClaim, got.Signals, test.want)
			}
		})
	}
}
