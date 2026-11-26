package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// readCodexLines writes lines as a rollout and returns its segments and their native wire.
func readCodexLines(t *testing.T, lines []string) ([]agents.HistorySegment, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-orphan.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var segs []agents.HistorySegment
	var wire strings.Builder
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "codex", SessionID: "sess-orphan", Path: path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		b, _ := proto.Marshal(seg.NativeLogs)
		wire.Write(b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return segs, wire.String()
}

// An output whose call the parser no longer holds, because its turn was aborted before the
// working directory changed, or never saw, is dropped: its call's working directory is unknown.
func TestCodexOutputWithNoKnownCallIsDropped(t *testing.T) {
	t.Parallel()
	_, wire := readCodexLines(t, []string{
		`{"timestamp":"2026-10-03T10:00:00Z","type":"session_meta","payload":{"id":"sess-orphan","cwd":` + jsonText(workPath("repo-a")) + `}}`,
		`{"timestamp":"2026-10-03T10:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"shell","arguments":"{\"cmd\":\"echo hi\"}"}}`,
		`{"timestamp":"2026-10-03T10:00:02Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t1"}}`,
		`{"timestamp":"2026-10-03T11:00:00Z","type":"turn_context","payload":{"turn_id":"t2","cwd":` + jsonText(workPath("repo-b")) + `}}`,
		`{"timestamp":"2026-10-03T11:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"LATE_ORG_A_OUTPUT"}}`,
		`{"timestamp":"2026-10-03T11:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-never","output":"ORPHAN_OUTPUT"}}`,
		`{"timestamp":"2026-10-03T11:00:03Z","type":"event_msg","payload":{"type":"user_message","message":"work in repo b"}}`,
	})
	if strings.Contains(wire, "LATE_ORG_A_OUTPUT") {
		t.Errorf("an output after an aborted turn and a cwd change was emitted")
	}
	if strings.Contains(wire, "ORPHAN_OUTPUT") {
		t.Errorf("an output whose call was never seen was emitted")
	}
	if !strings.Contains(wire, "work in repo b") {
		t.Errorf("the prompt in repo b is missing")
	}
}

// A call's pending record survives the bounded mid-session flush, so a long session's output
// after that flush is still kept.
func TestCodexOutputAfterBoundedFlushIsKept(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"timestamp":"2026-10-03T10:00:00Z","type":"session_meta","payload":{"id":"sess-orphan","cwd":` + jsonText(workPath("repo-a")) + `}}`,
		`{"timestamp":"2026-10-03T10:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-long","name":"shell","arguments":"{\"cmd\":\"echo hi\"}"}}`,
	}
	t0 := time.Date(2026, 10, 3, 10, 0, 2, 0, time.UTC)
	for i := range 270 {
		lines = append(lines, fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"msg-%d"}}`, t0.Add(time.Duration(i)*time.Second).Format(time.RFC3339), i))
	}
	lines = append(lines, `{"timestamp":"2026-10-03T11:00:00Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-long","output":"KEPT_LONG_OUTPUT"}}`)
	segs, wire := readCodexLines(t, lines)
	if len(segs) < 2 {
		t.Fatalf("got %d segments, want the bounded flush to split the session", len(segs))
	}
	if !strings.Contains(wire, "KEPT_LONG_OUTPUT") {
		t.Errorf("an output after the bounded flush was dropped")
	}
}

// A relative file_path is resolved against the call's own workdir, not the session's cwd.
func TestCodexRelativeFilePathUsesCallWorkdir(t *testing.T) {
	t.Parallel()
	args := jsonText(map[string]string{"workdir": workPath("orgA"), "file_path": "../orgB/x.go"})
	segs, _ := readCodexLines(t, []string{
		`{"timestamp":"2026-10-03T10:00:00Z","type":"session_meta","payload":{"id":"sess-orphan","cwd":` + jsonText(workPath("orgA/sub/deep")) + `}}`,
		`{"timestamp":"2026-10-03T10:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"read_file","arguments":` + jsonText(args) + `}}`,
		`{"timestamp":"2026-10-03T10:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"hi"}}`,
	})
	want := hookrun.Alone(workPath("orgB/x.go"))[0]
	var got []string
	for _, s := range segs {
		for _, r := range s.ToolReaches {
			got = append(got, r.Paths...)
		}
	}
	if !slices.Contains(got, want) {
		t.Errorf("reaches = %q, want %q", got, want)
	}
}
