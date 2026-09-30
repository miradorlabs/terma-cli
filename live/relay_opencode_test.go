package live

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// OpenCode through the relay: terma's plugin exports OTLP/JSON with session.id and
// calls terma's hooks itself, so this is the JSON path end to end.

const openCodePrompt = "Reply exactly TERMA_TELEMETRY_REPLY. TERMA_OPENCODE_PROMPT"

func openCodeAgentRecords(e telemetryEvidence, sid string) int {
	n := 0
	for _, r := range e.logs {
		if r.Resource["service.name"] == "opencode" && (sid == "" || r.Attrs["session.id"] == sid) {
			n++
		}
	}
	for _, s := range e.spans {
		if s.Resource["service.name"] == "opencode" && (sid == "" || s.Attrs["session.id"] == sid) {
			n++
		}
	}
	return n
}

func TestRelayOpenCode(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				sb := New(t, Isolated)
				var calls atomic.Int32
				provider := httptest.NewServer(openAIChatProvider(&calls))
				defer provider.Close()
				sb.UseOpenCodeProvider(provider.URL)
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				sid := sb.OpenCodeRun(b, sb.Repo, "", openCodePrompt)
				deadline := time.Now().Add(30 * time.Second)
				for openCodeAgentRecords(sb.Receiver.evidence(), sid) == 0 && time.Now().Before(deadline) {
					time.Sleep(500 * time.Millisecond)
				}
				time.Sleep(3 * time.Second)
				e := sb.Receiver.evidence()
				if openCodeAgentRecords(e, sid) == 0 {
					t.Fatalf("nothing of the opted-in OpenCode session reached upstream: %v", sb.RelayStats())
				}
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
				checkExportRequests(&failures, e, "/v1/logs", "/v1/traces")
				for _, f := range failures {
					t.Error(f)
				}
				leaked := leakedFieldsOf(e, "TERMA_OPENCODE_PROMPT", "TERMA_TELEMETRY_REPLY")
				if content && len(leaked) == 0 {
					t.Errorf("content allowed, but the prompt reached upstream nowhere")
				}
				if !content && len(leaked) > 0 {
					t.Errorf("content withheld, but it reached upstream in: %v", leaked)
				}
				sb.StopRelay()
				c := sb.RelayStats()
				noteRelayStats(t.Name(), c)
				failUnclassified(t, c)
			})
		}
	})
}

// OpenCode outside an opted-in repository reaches nothing upstream, while an opted-in
// session running alongside it on the same relay arrives.
//
// Continuing a session from another directory is not a resume-elsewhere case for
// OpenCode: a session belongs to its project, and `opencode run --session <id> --dir
// <other>` runs in the session's own project directory (1.18.33, --print-logs). It
// also never exits there after finishing its turn, terma or not, so it is not driven.
func TestRelayOpenCodeNegative(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		sb := New(t, Isolated)
		var calls atomic.Int32
		provider := httptest.NewServer(openAIChatProvider(&calls))
		defer provider.Close()
		sb.UseOpenCodeProvider(provider.URL)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second, Content: true})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		stray := sb.OpenCodeRun(b, personal, "", "TERMA_PERSONAL_WORK one")
		sid := sb.OpenCodeRun(b, sb.Repo, "", openCodePrompt)
		time.Sleep(8 * time.Second)
		e := sb.Receiver.evidence()
		if n := openCodeAgentRecords(e, stray); n != 0 {
			t.Errorf("an OpenCode session outside any repository reached upstream: %d records", n)
		}
		if leaked := leakedFieldsOf(e, "TERMA_PERSONAL_WORK"); len(leaked) > 0 {
			t.Errorf("personal work reached upstream: %v", leaked)
		}
		if openCodeAgentRecords(e, sid) == 0 {
			t.Errorf("the opted-in run itself never arrived: %v", sb.RelayStats())
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
	})
}
