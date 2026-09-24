package harness

import (
	"context"
	"strings"
	"testing"
)

func TestCodexDesktopActivityReadsUsageAndHostedActionsOnly(t *testing.T) {
	path := sequenceFixture(t, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + testCodexID + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurnA + `","trace_id":"` + replyTraceA + `"}}`,
		`{"type":"turn_context","payload":{"turn_id":"` + replyTurnA + `","model":"gpt-6-sol"}}`,
		`{"timestamp":"2026-09-19T18:18:40Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + replyTurnA + `","item":{"type":"CommandExecution","id":"item_local","command":"secret"}}}`,
		`{"timestamp":"2026-09-19T18:18:41Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + replyTurnA + `","started_at_ms":1790000000000,"completed_at_ms":1790000000450,"item":{"type":"Extension","id":"item_web","kind":"web.search","query":"local tools"}}}`,
		`{"timestamp":"2026-09-19T18:18:42Z","type":"token_usage_record","payload":{"turn_id":"` + replyTurnA + `","response_id":"resp_1","usage":{"input_tokens":100,"cached_input_tokens":25,"cache_write_input_tokens":5,"output_tokens":40,"reasoning_output_tokens":10}}}`,
		`{"timestamp":"2026-09-19T18:18:43Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + replyTurnA + `","started_at_ms":1790000001000,"completed_at_ms":1790000001220,"item":{"type":"ContextCompaction","id":"item_compact"}}}`,
		`{"timestamp":"2026-09-19T18:18:44Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurnA + `","started_at":1790000000,"duration_ms":4400,"time_to_first_token_ms":300}}`,
	}, "\n")+"\n")
	var got []CodexDesktopActivity
	read := func(c CodexDesktopCursor) (CodexDesktopCursor, string) {
		next, status, err := ReadCodexDesktopActivity(context.Background(), testCodexID, path, c, func(a CodexDesktopActivity) error {
			got = append(got, a)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return next, status
	}
	next, status := read(CodexDesktopCursor{})
	if status != "caught_up" || len(got) != 4 {
		t.Fatalf("status %q, activities %+v", status, got)
	}
	if got[0].Kind != "tool" || got[0].ID != "item_web" || got[0].ToolName != "Extension:web.search" || got[0].Input != "local tools" || !got[0].HasDuration || got[0].DurationMs != 450 || got[0].TraceID != replyTraceA {
		t.Fatalf("hosted action %+v", got[0])
	}
	if got[1].Kind != "model" || got[1].ID != "resp_1" || got[1].Model != "gpt-6-sol" || got[1].InputTokens != 100 || got[1].CachedInputTokens != 25 || got[1].OutputTokens != 40 {
		t.Fatalf("model usage %+v", got[1])
	}
	if got[2].Kind != "compaction" || got[2].ID != "item_compact" || got[2].DurationMs != 220 || got[3].Kind != "turn" || got[3].ID != replyTurnA || got[3].DurationMs != 4400 || got[3].TTFTMs != 300 {
		t.Fatalf("operation timing %+v", got)
	}
	_, status = read(next)
	if status != "caught_up" || len(got) != 4 {
		t.Fatalf("replayed acknowledged activity: %q %+v", status, got)
	}
}
