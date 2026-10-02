package daemon

import (
	"errors"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

func TestCapturePolicy(t *testing.T) {
	open := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true}
	global := open
	global.Mode = config.ModeGlobal
	excluding := open
	excluding.ExcludePaths = []string{"secrets"}
	nothing := open
	nothing.CollectsNothing = true
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
			want{true, true, true, nil}},
		{"the record narrows content", Capture{Org: open, Record: record(false, true, "traces", "logs"), Harness: "claude"},
			want{false, true, true, []string{"traces", "logs"}}},
		{"the record cannot widen content", Capture{Org: config.Policy{}, Record: record(true, true, "logs"), Harness: "claude"},
			want{false, false, true, []string{"logs"}}},
		{"the record picks the signals", Capture{Org: open, Record: record(true, true, "metrics"), Harness: "pi"},
			want{true, true, true, []string{"metrics"}}},
		{"an organization collecting nothing withholds the record's", Capture{Org: nothing, Record: record(true, true, "traces", "logs"), Harness: "pi"},
			want{false, false, true, []string{}}},
		{"an organization collecting nothing withholds without a record", Capture{Org: nothing, Harness: "claude"},
			want{false, false, true, []string{}}},
		{"an unreadable record withholds everything", Capture{Org: open, RecordErr: errors.New("torn"), Harness: "claude"},
			want{false, false, true, []string{}}},
		{"an agent the record does not name is withheld", Capture{Org: open, Record: record(true, true, "traces"), Harness: "codex"},
			want{false, false, true, []string{}}},
		{"catch-all has no agent to check", Capture{Org: global, Primary: true, Record: record(true, true, "traces")},
			want{true, true, false, []string{"traces"}}},
		{"path exclusions withhold content", Capture{Org: excluding, Harness: "claude"},
			want{false, false, true, nil}},
		{"global mode's own project needs no claim", Capture{Org: global, Primary: true, Harness: "claude"},
			want{true, true, false, nil}},
		{"another project in global mode needs one", Capture{Org: global, Harness: "claude"},
			want{true, true, true, nil}},
		{"a primary project out of global mode needs one", Capture{Org: open, Primary: true, Harness: "claude"},
			want{true, true, true, nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := CapturePolicy(test.in)
			if got.IncludePrompts != test.want.prompts || got.IncludeToolContent != test.want.tools || got.RequireClaim != test.want.requireClaim ||
				!slices.Equal(got.Signals, test.want.signals) || (got.Signals == nil) != (test.want.signals == nil) {
				t.Fatalf("got prompts=%v tools=%v requireClaim=%v signals=%#v, want %+v", got.IncludePrompts, got.IncludeToolContent, got.RequireClaim, got.Signals, test.want)
			}
			named := map[string]any{"file_path": map[string]any{"stringValue": "secrets/prod.env"}}
			if excluded := got.Excludes != nil && got.Excludes(named); excluded != (len(test.in.Org.ExcludePaths) > 0) {
				t.Fatalf("a value naming secrets/prod.env excluded=%v, want the organization's %v", excluded, test.in.Org.ExcludePaths)
			}
		})
	}
}
