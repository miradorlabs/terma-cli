package daemon

import (
	"errors"
	"os"
	"path/filepath"
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
	record := func(signals ...string) *routing.Record {
		return &routing.Record{Signals: signals, Harnesses: []string{"claude", "pi"}}
	}
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
		{"no record keeps the organization's policy", Capture{Org: open, Harness: "claude"},
			want{true, true, true, nil}},
		{"the record decides signals, never content", Capture{Org: open, Record: record("traces", "logs"), Harness: "claude"},
			want{true, true, true, []string{"traces", "logs"}}},
		{"the team's policy withholds content whatever the record", Capture{Org: teamOff, Record: record("logs"), Harness: "claude"},
			want{false, true, true, []string{"logs"}}},
		{"an unreadable record withholds everything", Capture{Org: open, RecordErr: errors.New("torn"), Harness: "claude"},
			want{false, false, true, []string{}}},
		{"an agent the record does not name is withheld", Capture{Org: open, Record: record("traces"), Harness: "codex"},
			want{false, false, true, []string{}}},
		{"catch-all has no agent to check", Capture{Org: global, Primary: true, Record: record("traces")},
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

// A record an earlier terma wrote with content off narrows nothing: content is the team
// policy's alone.
func TestAnOldRecordsContentSwitchesNarrowNothing(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	dir, err := routing.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"project_id":"p1","signals":["logs"],"include_prompts":false,"include_tool_content":false,"harnesses":["claude"]}`
	if err := os.WriteFile(filepath.Join(dir, "p1.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := routing.LoadRecord("p1")
	if err != nil || !ok {
		t.Fatalf("LoadRecord = %v, %v", ok, err)
	}
	got := CapturePolicy(Capture{Org: config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true}, Record: &rec, Harness: "claude"})
	if !got.IncludePrompts || !got.IncludeToolContent || !slices.Equal(got.Signals, []string{"logs"}) {
		t.Fatalf("an old record narrowed content: %+v", got)
	}
}

// A session claimed in an excluded workspace sends nothing; hooks no longer check.
func TestCapturePolicyDropsAnExcludedWorkspace(t *testing.T) {
	org := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true, ExcludePaths: []string{"/w/secret/**"}}
	for root, want := range map[string]bool{"/w/secret/repo": true, "/w/open": false, "": false} {
		if got := CapturePolicy(Capture{Org: org, Harness: "claude", Root: root}).ExcludedWorkspace; got != want {
			t.Errorf("root %q: ExcludedWorkspace = %v, want %v", root, got, want)
		}
	}
}
