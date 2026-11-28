package codex

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func TestCodexDiscoverHistoryFiltersDeduplicatesAndMarksActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	writeRollout := func(rel, sid string, mtime time.Time) {
		t.Helper()
		full := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		line := `{"timestamp":"2026-10-03T22:01:34.068Z","type":"session_meta","payload":{"id":"` + sid + `","cwd":"/work/a","originator":"codex_cli_rs","cli_version":"0.159.3"}}` + "\n"
		if err := os.WriteFile(full, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	// Same session in both sessions/ and archived_sessions/ — deduplicates keeping newer.
	writeRollout("sessions/2026/11/20/rollout-2026-11-20T10-00-00-11111111-1111-1111-1111-111111111111.jsonl", "11111111-1111-1111-1111-111111111111", now.Add(-5*24*time.Hour))
	writeRollout("archived_sessions/2026/11/20/rollout-2026-11-20T10-00-00-11111111-1111-1111-1111-111111111111.jsonl", "11111111-1111-1111-1111-111111111111", now.Add(-4*24*time.Hour))
	// Active session (15 minutes ago).
	writeRollout("sessions/2026/11/25/rollout-2026-11-25T11-45-00-22222222-2222-2222-2222-222222222222.jsonl", "22222222-2222-2222-2222-222222222222", now.Add(-15*time.Minute))
	// Older than 90d.
	writeRollout("sessions/2026/06/01/rollout-2026-06-01T10-00-00-33333333-3333-3333-3333-333333333333.jsonl", "33333333-3333-3333-3333-333333333333", now.Add(-120*24*time.Hour))

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{
		Since:        now.Add(-90 * 24 * time.Hour),
		Now:          now,
		ActiveWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("discovered %d files, want 3 (deduplicated)", len(files))
	}
	byID := map[string]agents.HistoryFile{}
	for _, f := range files {
		byID[f.SessionID] = f
	}
	if f := byID["11111111-1111-1111-1111-111111111111"]; f.Active || f.OutsideSince || !strings.Contains(f.Path, "archived_sessions") {
		t.Errorf("deduplicated file = %+v, want archived_sessions copy", f)
	}
	if f := byID["22222222-2222-2222-2222-222222222222"]; !f.Active || f.OutsideSince {
		t.Errorf("active file = %+v, want Active=true", f)
	}
	if f := byID["33333333-3333-3333-3333-333333333333"]; f.Active || !f.OutsideSince {
		t.Errorf("old file = %+v, want OutsideSince=true", f)
	}
}

func TestCodexReadHistoryResumedRolloutFidelityAndContentFilter(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex_rollout_resumed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	content := strings.NewReplacer(
		"{{ID}}", "019d2b54-7a10-7000-8000-000000000099",
		"{{UNLISTED}}", "/work/repo-a",
		"{{LISTED}}", "/work/repo-b",
	).Replace(string(raw))
	path := filepath.Join(t.TempDir(), "rollout-resumed.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var segs []agents.HistorySegment
	err = (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "codex",
		SessionID:    "019d2b54-7a10-7000-8000-000000000099",
		Path:         path,
	}, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (/work/repo-a, /work/repo-b)", len(segs))
	}
	if segs[0].Cwd != "/work/repo-a" || segs[1].Cwd != "/work/repo-b" {
		t.Fatalf("segment cwds = [%q, %q]", segs[0].Cwd, segs[1].Cwd)
	}

	// Verify segment 0 native logs match Codex's live OTLP log structure.
	rl := segs[0].NativeLogs.ResourceLogs[0]
	if got := cxStr(rl.Resource.Attributes, "service.name"); got != "codex_exec" {
		t.Errorf("service.name = %q, want codex_exec", got)
	}
	if got := cxStr(rl.Resource.Attributes, "service.version"); got != "0.159.3" {
		t.Errorf("service.version = %q, want 0.159.3", got)
	}
	if !cxBoolTrue(rl.Resource.Attributes, semconv.TermaSessionHistoricalKey) {
		t.Errorf("resource missing %s=true", semconv.TermaSessionHistoricalKey)
	}
	sl := rl.ScopeLogs[0]
	if sl.Scope.Name != "codex_otel.log_only" {
		t.Errorf("scope = %q, want codex_otel.log_only", sl.Scope.Name)
	}

	counts := map[string]int{}
	var modelCalls []*logspb.LogRecord
	var prompts []*logspb.LogRecord
	for _, lr := range sl.LogRecords {
		if !cxBoolTrue(lr.Attributes, semconv.TermaSessionHistoricalKey) {
			t.Errorf("record missing %s=true", semconv.TermaSessionHistoricalKey)
		}
		name := cxStr(lr.Attributes, "event.name")
		counts[name]++
		if name == "codex.sse_event" {
			modelCalls = append(modelCalls, lr)
		}
		if name == "codex.user_prompt" {
			prompts = append(prompts, lr)
		}
	}
	// Turn 1 has 1 conversation_starts, 1 user_prompt (not duplicated!), 2 token_usage_records,
	// and 2 tool_result records (inner exec_command + outer code-mode exec wrapper, matching live Codex 0.160+).
	if counts["codex.conversation_starts"] != 1 || counts["codex.user_prompt"] != 1 || counts["codex.sse_event"] != 2 || counts["codex.tool_result"] != 2 {
		t.Fatalf("segment 0 event counts = %v, want conversation_starts=1 user_prompt=1 sse_event=2 tool_result=2", counts)
	}
	if got := hex.EncodeToString(prompts[0].TraceId); got != "1bfee82c6a26cd8a5d2d851536d8d50c" {
		t.Errorf("prompt trace_id = %q, want 1bfee82c6a26cd8a5d2d851536d8d50c", got)
	}
	if modelCalls[0].TimeUnixNano == modelCalls[1].TimeUnixNano {
		t.Errorf("model calls share identical TimeUnixNano (%d), would collide on platform EventId", modelCalls[0].TimeUnixNano)
	}
	if got := cxInt(modelCalls[0].Attributes, "input_token_count"); got != 15321 {
		t.Errorf("model call 0 input_token_count = %d, want 15321", got)
	}
	if got := cxInt(modelCalls[0].Attributes, "cached_token_count"); got != 12288 {
		t.Errorf("model call 0 cached_token_count = %d, want 12288", got)
	}
	if got := cxInt(modelCalls[0].Attributes, "output_token_count"); got != 63 {
		t.Errorf("model call 0 output_token_count = %d, want 63", got)
	}
	if got := cxInt(modelCalls[0].Attributes, "tool_token_count"); got != 15384 {
		t.Errorf("model call 0 tool_token_count = %d, want 15384", got)
	}

	// Supplemental assistant replies should exist for segment 0 and contain no repo-b text.
	if segs[0].SupplementalLogs == nil || len(segs[0].SupplementalLogs.ResourceLogs[0].ScopeLogs[0].LogRecords) != 2 {
		t.Fatalf("segment 0 supplemental replies = %+v, want 2", segs[0].SupplementalLogs)
	}
	wireA, _ := proto.Marshal(segs[0].NativeLogs)
	wireB, _ := proto.Marshal(segs[1].NativeLogs)
	if strings.Contains(string(wireA), "P8 LISTEDCODEX-B") {
		t.Errorf("segment 0 (repo-a) leaked repo-b content")
	}
	if strings.Contains(string(wireB), "P8 UNLISTEDCODEX-B") {
		t.Errorf("segment 1 (repo-b) leaked repo-a content")
	}

	// Content filter under restrictive policy leaves zero unclassified keys, redacts prompts,
	// drops tool arguments/output, and keeps terma.session.historical=true.
	filter := relay.NewContentFilter([]shape.Capturer{Agent{}})
	filtered := proto.CloneOf(segs[0].NativeLogs)
	_, unclassified := filter.FilterLogs(filtered, false, false)
	if len(unclassified) != 0 {
		t.Fatalf("restrictive filter reported unclassified keys: %v", unclassified)
	}
	fWire, _ := proto.Marshal(filtered)
	if strings.Contains(string(fWire), "UNLISTEDCODEX-B") {
		t.Fatalf("withheld prompt/tool content survived content filter")
	}
	for _, lr := range filtered.ResourceLogs[0].ScopeLogs[0].LogRecords {
		if !cxBoolTrue(lr.Attributes, semconv.TermaSessionHistoricalKey) {
			t.Errorf("historical marker dropped by content filter")
		}
		if cxStr(lr.Attributes, "event.name") == "codex.user_prompt" && cxStr(lr.Attributes, "prompt") != "[REDACTED]" {
			t.Errorf("withheld codex prompt = %q, want [REDACTED]", cxStr(lr.Attributes, "prompt"))
		}
	}
}

func TestCodexReadHistoryLegacyTokenCountAndMissingCwd(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"timestamp":"2026-09-01T10:00:00Z","type":"session_meta","payload":{"id":"sess-legacy","originator":"codex_cli_rs","cli_version":"0.140.0"}}`,
		`{"timestamp":"2026-09-01T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello legacy"}}`,
		`{"timestamp":"2026-09-01T10:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":550},"last_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":550}}}}`,
		// Duplicate rate-limit snapshot with identical total_tokens must not emit a second call.
		`{"timestamp":"2026-09-01T10:00:02.500Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":550},"last_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":550}}}}`,
	}
	path := filepath.Join(t.TempDir(), "rollout-legacy.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var segs []agents.HistorySegment
	err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "codex",
		SessionID:    "sess-legacy",
		Path:         path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || segs[0].Cwd != "" {
		t.Fatalf("got %d segments (cwd=%q), want 1 segment with empty cwd", len(segs), segs[0].Cwd)
	}
	sseCount := 0
	for _, lr := range segs[0].NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords {
		if cxStr(lr.Attributes, "event.name") == "codex.sse_event" {
			sseCount++
		}
	}
	if sseCount != 1 {
		t.Fatalf("legacy token_count emitted %d model calls, want 1", sseCount)
	}
}

func TestCodexToolFailureTraceResetAndRepeatedLegacyPrompts(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"timestamp":"2026-10-03T22:01:00Z","type":"session_meta","payload":{"id":"sess-edge","cwd":"/work/a","originator":"codex_cli_rs"}}`,
		`{"timestamp":"2026-10-03T22:01:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1","trace_id":"1bfee82c6a26cd8a5d2d851536d8d50c"}}`,
		`{"timestamp":"2026-10-03T22:01:02Z","type":"event_msg","payload":{"type":"user_message","message":"continue"}}`,
		`{"timestamp":"2026-10-03T22:01:03Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call-fail","name":"exec","input":"false"}}`,
		`{"timestamp":"2026-10-03T22:01:04Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-fail","command":["/bin/sh","-c","false"],"status":"failed","exit_code":1}}}`,
		`{"timestamp":"2026-10-03T22:01:05Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-fail","output":[{"type":"input_text","text":"{\"exit_code\":1,\"output\":\"boom\"}"}]}}`,
		`{"timestamp":"2026-10-03T22:01:06Z","type":"token_usage_record","payload":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		// Turn 2 starts via turn_context without task_complete: trace_id MUST clear!
		`{"timestamp":"2026-10-03T22:02:00Z","type":"turn_context","payload":{"turn_id":"turn-2","cwd":"/work/a"}}`,
		`{"timestamp":"2026-10-03T22:02:01Z","type":"event_msg","payload":{"type":"user_message","message":"continue"}}`,
	}
	path := filepath.Join(t.TempDir(), "rollout-edge.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var segs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "codex", SessionID: "sess-edge", Path: path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var prompts, tools []*logspb.LogRecord
	for _, lr := range segs[0].NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords {
		switch cxStr(lr.Attributes, "event.name") {
		case "codex.user_prompt":
			prompts = append(prompts, lr)
		case "codex.tool_result":
			tools = append(tools, lr)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tool_result records, want 2 (exec_command + exec wrapper)", len(tools))
	}
	for i, tr := range tools {
		if cxBoolTrue(tr.Attributes, "success") {
			t.Errorf("failing tool call %d (%s) recorded success=true, want false", i, cxStr(tr.Attributes, "tool_name"))
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2 (second 'continue' preserved)", len(prompts))
	}
	if len(prompts[1].TraceId) != 0 {
		t.Errorf("turn 2 prompt inherited stale turn 1 TraceId %x", prompts[1].TraceId)
	}
}

func TestCodexCrossCwdToolArgsDroppedAndResumedStartUsesSegmentTimestamp(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"timestamp":"2026-10-03T10:00:00Z","type":"session_meta","payload":{"id":"sess-xorg","timestamp":"2026-10-03T10:00:00Z","cwd":` + jsonText(workPath("repo-a")) + `}}`,
		`{"timestamp":"2026-10-03T10:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"shell","arguments":"{\"cmd\":\"echo SECRET_ORG_A_ARG\"}"}}`,
		`{"timestamp":"2026-10-03T10:00:02Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"fc-a","changes":[{"path":` + jsonText(workPath("repo-a/touched.go")) + `}]}}}`,
		`{"timestamp":"2026-10-03T11:00:00Z","type":"turn_context","payload":{"turn_id":"t2","cwd":` + jsonText(workPath("repo-b")) + `}}`,
		`{"timestamp":"2026-10-03T11:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"SECRET_ORG_A_OUTPUT"}}`,
		`{"timestamp":"2026-10-03T11:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"work in repo b"}}`,
		`{"timestamp":"2026-10-03T11:00:03Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-b","command":["/bin/sh","-c","cat rel.go"],"cwd":` + jsonText(fileURL(workPath("repo-c"))) + `,"status":"completed","exit_code":0}}}`,
		`{"timestamp":"2026-10-03T11:00:04Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"fc-1","changes":[{"path":` + jsonText(workPath("repo-d/patched.go")) + `}]}}}`,
	}
	path := filepath.Join(t.TempDir(), "rollout-xorg.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var segs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "codex", SessionID: "sess-xorg", Path: path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// repo-a's turn produced no record of its own, so it is no segment.
	if len(segs) != 1 || segs[0].Cwd != workPath("repo-b") {
		t.Fatalf("got %d segments %+v, want one in %s", len(segs), segs, workPath("repo-b"))
	}
	wire, _ := proto.Marshal(segs[0].NativeLogs)
	if strings.Contains(string(wire), "SECRET_ORG_A") {
		t.Fatalf("org A tool arguments or delayed output leaked into org B segment")
	}
	wantStart := time.Date(2026, 10, 3, 11, 0, 2, 0, time.UTC).UnixNano()
	for _, lr := range segs[0].NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords {
		if cxStr(lr.Attributes, "event.name") == "codex.conversation_starts" && int64(lr.TimeUnixNano) != wantStart {
			t.Errorf("resumed conversation_starts TimeUnixNano = %d, want %d (segment time, not repo-a metaTime)", lr.TimeUnixNano, wantStart)
		}
	}
}

func TestCodexDiscoverHistoryNamesASubagentsParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("WORKSPACE", workPath("repo-a"))

	sessDir := filepath.Join(home, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	parentID := "019d2b54-7a10-7000-8000-000000000101"
	childID := "019d2b54-7a10-7000-8000-000000000102"
	parentPath := filepath.Join(sessDir, "rollout-2026-10-03T10-00-00-"+parentID+".jsonl")
	childPath := filepath.Join(sessDir, "rollout-2026-10-03T10-01-00-"+childID+".jsonl")

	// function_call arguments is a JSON string that holds JSON, so it is encoded twice.
	argvArgs := jsonText(jsonText(map[string]any{
		"command": []string{"/bin/sh", "-c", "cat " + filepath.ToSlash(workPath("repo-b/secret.go"))},
		"workdir": workPath("repo-b"),
	}))
	patchArgs := jsonText(jsonText(map[string]any{
		"input": "*** Begin Patch\n*** Update File: " + workPath("repo-c/patch.go") + "\n@@\n-a\n+b\n*** End Patch\n",
	}))
	parentLines := []string{
		`{"timestamp":"2026-10-03T10:00:00Z","type":"session_meta","payload":{"id":"` + parentID + `","cwd":` + jsonText(workPath("repo-a")) + `,"git":{"branch":"feature-a"}}}`,
		`{"timestamp":"2026-10-03T10:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"c-argv","name":"shell","arguments":` + argvArgs + `}}`,
		`{"timestamp":"2026-10-03T10:00:02Z","type":"response_item","payload":{"type":"function_call","call_id":"c-patch","name":"apply_patch","arguments":` + patchArgs + `}}`,
		`{"timestamp":"2026-10-03T10:00:03Z","type":"response_item","payload":{"type":"function_call","call_id":"c-env","name":"exec_command","arguments":"{\"cmd\":\"cat $WORKSPACE/notes\"}"}}`,
		`{"timestamp":"2026-10-03T10:00:04Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"fc-bare","path":` + jsonText(workPath("repo-d/bare.go")) + `}}}`,
	}
	childLines := []string{
		`{"timestamp":"2026-10-03T10:01:00Z","type":"session_meta","payload":{"id":"` + childID + `","cwd":` + jsonText(workPath("repo-a")) + `,"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `","depth":1}}}}}`,
		`{"timestamp":"2026-10-03T10:01:05Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"fc-child","path":` + jsonText(workPath("repo-b/from-child.go")) + `}}}`,
	}
	for p, lines := range map[string][]string{parentPath: parentLines, childPath: childLines} {
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-3 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var gotChild agents.HistoryFile
	for _, f := range files {
		if f.SessionID == childID {
			gotChild = f
		}
	}
	if gotChild.ParentSessionID != parentID {
		t.Fatalf("child ParentSessionID = %q, want %q", gotChild.ParentSessionID, parentID)
	}
}

func cxStr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func cxInt(attrs []*commonpb.KeyValue, key string) int64 {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetIntValue()
		}
	}
	return -1
}

func cxBoolTrue(attrs []*commonpb.KeyValue, key string) bool {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			b, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_BoolValue)
			return ok && b.BoolValue
		}
	}
	return false
}

// workPath returns rel (written with forward slashes) under a made-up absolute root, as
// this operating system writes one: /work/<rel> on Unix, C:\work\<rel> on Windows.
func workPath(rel string) string {
	if runtime.GOOS == "windows" {
		return `C:\work\` + filepath.FromSlash(rel)
	}
	return "/work/" + rel
}

// fileURL returns the file:// URL for the absolute path p.
func fileURL(p string) string {
	s := filepath.ToSlash(p)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return "file://" + s
}

// jsonText returns v encoded as JSON, for splicing into a fixture line.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestFileURLPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw     string
		windows bool
		want    string
	}{
		{"file:///work/repo-c", false, "/work/repo-c"},
		{"file:///work/my%20repo", false, "/work/my repo"},
		{"file:///C:/Users/foo/repo", true, `C:\Users\foo\repo`},
		{"file:///c%3A/Users/foo/repo", true, `c:\Users\foo\repo`},
		{"file://localhost/C:/Users/foo", true, `C:\Users\foo`},
		{"file://server/share/dir", true, `\\server\share\dir`},
		{"file:///work/repo-c", true, `\work\repo-c`},
	} {
		if got := fileURLPath(tc.raw, tc.windows); got != tc.want {
			t.Errorf("fileURLPath(%q, windows=%v) = %q, want %q", tc.raw, tc.windows, got, tc.want)
		}
	}
	if got := codexNormalizeCwd(fileURL(workPath("repo-c")), workPath("repo-a")); got != workPath("repo-c") {
		t.Errorf("codexNormalizeCwd(%q) = %q, want %q", fileURL(workPath("repo-c")), got, workPath("repo-c"))
	}
}

func TestCodexNormalizeCwdJoinsARelativeCwd(t *testing.T) {
	if got := codexNormalizeCwd("sub", "/work/a"); got != filepath.Join("/work/a", "sub") {
		t.Errorf("codexNormalizeCwd(sub, /work/a) = %q", got)
	}
	if got := codexNormalizeCwd("sub", ""); got != "sub" {
		t.Errorf("codexNormalizeCwd(sub, \"\") = %q", got)
	}
}

func TestCodexUntimedPromptStaysUntimed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sessDir := filepath.Join(home, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "019d2b54-7a10-7000-8000-000000000201"
	path := filepath.Join(sessDir, "rollout-2026-10-03T10-00-00-"+sid+".jsonl")
	lines := []string{
		`{"timestamp":"2026-10-03T10:00:00Z","type":"session_meta","payload":{"id":"` + sid + `","cwd":"/work/repo-a"}}`,
		`{"timestamp":"2026-10-03T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"first"}}`,
		`{"timestamp":"2026-10-03T10:00:02Z","type":"response_item","payload":{"type":"function_call","call_id":"c-read","name":"exec_command","arguments":"{\"cmd\":\"cat /work/repo-b/secret.go\"}"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"UNTIMED_AFTER_READ"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := false
	err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{SourceSystem: codexSourceSystem, SessionID: sid, Path: path}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		for _, rl := range seg.NativeLogs.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					if cxStr(lr.Attributes, "prompt") != "UNTIMED_AFTER_READ" {
						continue
					}
					found = true
					// An untimed prompt stays untimed rather than taking a made-up time.
					if lr.TimeUnixNano != 0 || lr.ObservedTimeUnixNano != 0 {
						t.Errorf("untimed prompt got time %d/%d, want 0", lr.TimeUnixNano, lr.ObservedTimeUnixNano)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("untimed prompt was not emitted")
	}
}
