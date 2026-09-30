package relay

import (
	"errors"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

func TestCapturePolicy(t *testing.T) {
	open := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true, Signals: []string{"traces", "logs", "metrics"}}
	global := open
	global.Mode = config.ModeGlobal
	excluding := open
	excluding.ExcludePaths = []string{"secrets"}
	narrowOrg := open
	narrowOrg.Signals = []string{"logs"}
	allSignalsOrg := open
	allSignalsOrg.Signals = nil
	record := func(prompts, tools bool, signals ...string) *routing.Record {
		return &routing.Record{IncludePrompts: prompts, IncludeToolContent: tools, Signals: signals, Harnesses: []string{"claude", "pi"}}
	}
	type want struct {
		prompts, tools, requireClaim bool
		signals                      []string
	}
	for _, test := range []struct {
		name string
		in   Capture
		want want
	}{
		{"no record keeps the organization's policy", Capture{Org: open, Harness: "claude"},
			want{true, true, true, []string{"traces", "logs", "metrics"}}},
		{"the record narrows content", Capture{Org: open, Record: record(false, true, "traces", "logs"), Harness: "claude"},
			want{false, true, true, []string{"traces", "logs"}}},
		{"the record cannot widen content", Capture{Org: config.Policy{Signals: []string{"logs"}}, Record: record(true, true, "logs"), Harness: "claude"},
			want{false, false, true, []string{"logs"}}},
		{"a signal needs the organization and the record", Capture{Org: narrowOrg, Record: record(true, true, "traces", "logs"), Harness: "pi"},
			want{true, true, true, []string{"logs"}}},
		{"an organization naming no signals allows the record's", Capture{Org: allSignalsOrg, Record: record(true, true, "metrics"), Harness: "pi"},
			want{true, true, true, []string{"metrics"}}},
		{"an unreadable record withholds everything", Capture{Org: open, RecordErr: errors.New("torn"), Harness: "claude"},
			want{false, false, true, []string{}}},
		{"an agent the record does not name is withheld", Capture{Org: open, Record: record(true, true, "traces"), Harness: "codex"},
			want{false, false, true, []string{}}},
		{"catch-all has no agent to check", Capture{Org: global, Primary: true, Record: record(true, true, "traces")},
			want{true, true, false, []string{"traces"}}},
		{"path exclusions withhold content", Capture{Org: excluding, Harness: "claude"},
			want{false, false, true, []string{"traces", "logs", "metrics"}}},
		{"global mode's own project needs no claim", Capture{Org: global, Primary: true, Harness: "claude"},
			want{true, true, false, []string{"traces", "logs", "metrics"}}},
		{"another project in global mode needs one", Capture{Org: global, Harness: "claude"},
			want{true, true, true, []string{"traces", "logs", "metrics"}}},
		{"a primary project out of global mode needs one", Capture{Org: open, Primary: true, Harness: "claude"},
			want{true, true, true, []string{"traces", "logs", "metrics"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := CapturePolicy(test.in)
			if got.IncludePrompts != test.want.prompts || got.IncludeToolContent != test.want.tools || got.RequireClaim != test.want.requireClaim ||
				!slices.Equal(got.Signals, test.want.signals) || (got.Signals == nil) != (test.want.signals == nil) {
				t.Fatalf("got prompts=%v tools=%v requireClaim=%v signals=%#v, want %+v", got.IncludePrompts, got.IncludeToolContent, got.RequireClaim, got.Signals, test.want)
			}
			if !slices.Equal(got.ExcludePaths, test.in.Org.ExcludePaths) {
				t.Fatalf("exclusions %v, want the organization's %v", got.ExcludePaths, test.in.Org.ExcludePaths)
			}
		})
	}
}
