package cmd

import (
	"errors"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// A harness can be wired to the right host and still send this project's spend
// somewhere else. `terma doctor` fails on that; status must agree with it, or the
// two commands disagree about whether the setup works.
func TestHarnessState(t *testing.T) {
	const otlp = "https://otel-dev.mirador.org"
	const project = "6796a71f-7949-40f1-bde8-b87a74071686"

	cases := []struct {
		name    string
		st      harness.Status
		err     error
		project string
		want    string
		ok      bool
	}{
		{
			name: "connected to this project",
			st:   harness.Status{Connected: true, Endpoint: otlp, ProjectID: project, Signals: harness.AllSignals},
			want: "→ connected", ok: true, project: project,
		},
		{
			name: "connected, no project selected to compare against",
			st:   harness.Status{Connected: true, Endpoint: otlp, ProjectID: project, Signals: harness.AllSignals},
			want: "→ connected", ok: true, project: "",
		},
		{
			// Pointed at Terma and holding a key, but exporting nothing of its own:
			// only a repository's committed policy can make this send. Connected is
			// the truth; "connected" alone is not.
			name: "connected with every exporter off",
			st:   harness.Status{Connected: true, Endpoint: otlp, ProjectID: project},
			want: "→ connected; repositories decide what is sent", ok: true, project: project,
		},
		{
			name: "exporting to another project is not connected",
			st:   harness.Status{Connected: true, Endpoint: otlp, ProjectID: "c970664b-ba35-4cdd-b7a9-d5acadb327f6"},
			want: "→ reporting to project c970664b-ba35-4cdd-b7a9-d5acadb327f6, not this one — run `terma install`",
			ok:   false, project: project,
		},
		{
			name: "another collector entirely",
			st:   harness.Status{Connected: true, Endpoint: "https://otel.example.com", ProjectID: project},
			want: "→ not connected", ok: false, project: project,
		},
		{
			name: "not connected",
			st:   harness.Status{},
			want: "→ not connected", ok: false, project: project,
		},
		{
			name: "unreadable configuration",
			err:  errors.New("permission denied"),
			want: "(error)", ok: false, project: project,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := statusAgent(judgeHarness(harnessFacts{status: tc.st, err: tc.err}, otlp, tc.project), false)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("statusAgent = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
