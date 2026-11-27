package omp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func TestOmpDiscoverHistorySubagentsForksContinuationsAndOrphans(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	writeFile := func(rel, content string, mtime time.Time) string {
		t.Helper()
		full := filepath.Join(agentDir, "sessions", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return full
	}

	proj := "--work-repo-a--"
	// 1. Completed parent session with direct subagent + nested grandchild subagent.
	parentRel := filepath.Join(proj, "2026-11-20T10-00-00-000Z_parent1.jsonl")
	parentPath := writeFile(parentRel,
		`{"type":"title","title":"Parent title","updatedAt":"2026-11-20T10:01:00Z"}`+"\n"+
			`{"type":"session","version":3,"id":"parent1","timestamp":"2026-11-20T10:00:00Z","cwd":"/work/repo-a"}`+"\n",
		now.Add(-5*24*time.Hour),
	)
	subPath := writeFile(filepath.Join(proj, "2026-11-20T10-00-00-000Z_parent1", "0-explore.jsonl"),
		`{"type":"session","version":3,"id":"sub1","timestamp":"2026-11-20T10:00:30Z","cwd":"/work/repo-a","parentSession":`+mustJSON(parentPath)+`}`+"\n",
		now.Add(-5*24*time.Hour+time.Minute),
	)
	grandPath := writeFile(filepath.Join(proj, "2026-11-20T10-00-00-000Z_parent1", "0-explore", "0-finder.jsonl"),
		`{"type":"session","version":3,"id":"grand1","timestamp":"2026-11-20T10:00:45Z","cwd":"/work/repo-a","parentSession":`+mustJSON(subPath)+`}`+"\n",
		now.Add(-5*24*time.Hour+2*time.Minute),
	)

	// 2. Sibling fork (parentSession points to parentPath, lives at top level — NOT a subagent).
	writeFile(filepath.Join(proj, "2026-11-21T10-00-00-000Z_fork1.jsonl"),
		`{"type":"session","version":3,"id":"fork1","timestamp":"2026-11-21T10:00:00Z","cwd":"/work/repo-a","parentSession":`+mustJSON(parentPath)+`}`+"\n",
		now.Add(-4*24*time.Hour),
	)

	// 3. Sibling continuation (continuationOf — independent top-level session).
	writeFile(filepath.Join(proj, "2026-11-22T10-00-00-000Z_cont1.jsonl"),
		`{"type":"session","version":3,"id":"cont1","timestamp":"2026-11-22T10:00:00Z","cwd":"/work/repo-a","continuationOf":"parent1"}`+"\n",
		now.Add(-3*24*time.Hour),
	)

	// 4. Active parent via recent subagent mtime (parent old, subagent written 10m ago).
	activeParentRel := filepath.Join(proj, "2026-11-24T10-00-00-000Z_active1.jsonl")
	activeParentPath := writeFile(activeParentRel,
		`{"type":"session","version":3,"id":"active1","timestamp":"2026-11-24T10:00:00Z","cwd":"/work/repo-a"}`+"\n",
		now.Add(-24*time.Hour),
	)
	writeFile(filepath.Join(proj, "2026-11-24T10-00-00-000Z_active1", "0-worker.jsonl"),
		`{"type":"session","version":3,"id":"active-sub1","timestamp":"2026-11-25T11:50:00Z","cwd":"/work/repo-a","parentSession":`+mustJSON(activeParentPath)+`}`+"\n",
		now.Add(-10*time.Minute),
	)

	// 5. Session older than --since.
	writeFile(filepath.Join(proj, "2026-06-01T10-00-00-000Z_old1.jsonl"),
		`{"type":"session","version":3,"id":"old1","timestamp":"2026-06-01T10:00:00Z","cwd":"/work/repo-a"}`+"\n",
		now.Add(-120*24*time.Hour),
	)

	// 6. Orphaned subagent directory whose parent .jsonl was deleted.
	missingParent := filepath.Join(agentDir, "sessions", proj, "2026-11-23T10-00-00-000Z_goneparent.jsonl")
	writeFile(filepath.Join(proj, "2026-11-23T10-00-00-000Z_goneparent", "0-orphan.jsonl"),
		`{"type":"session","version":3,"id":"orphan1","timestamp":"2026-11-23T10:00:10Z","cwd":"/work/repo-a","parentSession":`+mustJSON(missingParent)+`}`+"\n",
		now.Add(-2*24*time.Hour),
	)

	// 7. Symlinked .jsonl must be skipped.
	symTarget := filepath.Join(t.TempDir(), "escape.jsonl")
	if err := os.WriteFile(symTarget, []byte(`{"type":"session","id":"sym1","cwd":"/work/repo-a"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(symTarget, filepath.Join(agentDir, "sessions", proj, "2026-11-20T11-00-00-000Z_sym1.jsonl"))

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{
		Since:        now.Add(-90 * 24 * time.Hour),
		Now:          now,
		ActiveWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]agents.HistoryFile{}
	for _, f := range files {
		byID[f.SessionID] = f
	}
	if _, ok := byID["sym1"]; ok {
		t.Errorf("symlinked session should not be discovered")
	}
	if _, ok := byID["sub1"]; ok {
		t.Errorf("attached subagent should not be a separate HistoryFile")
	}
	if _, ok := byID["grand1"]; ok {
		t.Errorf("nested subagent should not be a separate HistoryFile")
	}

	p, ok := byID["parent1"]
	if !ok {
		t.Fatalf("missing parent1; discovered keys: %v", mapKeys(byID))
	}
	if p.SourceSystem != "omp" || p.Active || p.OutsideSince || p.ParentSessionID != "" {
		t.Errorf("parent1 = %+v", p)
	}
	wantExtras := []string{subPath, grandPath}
	if !slices.Equal(p.ExtraPaths, wantExtras) {
		t.Errorf("parent1 ExtraPaths = %v, want %v (parent-first)", p.ExtraPaths, wantExtras)
	}
	if len(p.WatchDirs) < 2 {
		t.Errorf("parent1 WatchDirs = %v, want stem and nested subagent dir", p.WatchDirs)
	}

	if fk, ok := byID["fork1"]; !ok || fk.ParentSessionID != "parent1" {
		t.Errorf("fork1 should be independent top-level session with ParentSessionID=parent1, got %+v", fk)
	}
	if ct, ok := byID["cont1"]; !ok || ct.ParentSessionID != "parent1" {
		t.Errorf("cont1 should be independent top-level session with ParentSessionID=parent1, got %+v", ct)
	}
	if ac, ok := byID["active1"]; !ok || !ac.Active {
		t.Errorf("active1 should be Active due to recent subagent mtime, got %+v", ac)
	}
	if old, ok := byID["old1"]; !ok || !old.OutsideSince || old.Active {
		t.Errorf("old1 should be OutsideSince, got %+v", old)
	}
	if orph, ok := byID["orphan1"]; !ok || orph.ParentSessionID != "goneparent" {
		t.Errorf("orphan1 should carry ParentSessionID=goneparent for fail-closed handling, got %+v", orph)
	}
}

func TestOmpDiscoverHistoryUnreadableSubagentDirSetsCompanionReadError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("chmod 0o000 permissions require non-root Unix")
	}
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	projDir := filepath.Join(agentDir, "sessions", "--work-repo--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(projDir, "2026-11-20T10-00-00-000Z_locked1.jsonl")
	if err := os.WriteFile(parent, []byte(`{"type":"session","version":3,"id":"locked1","timestamp":"2026-11-20T10:00:00Z","cwd":"/work/repo"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(parent, old, old)

	subDir := filepath.Join(projDir, "2026-11-20T10-00-00-000Z_locked1")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(subDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(subDir, 0o700) })

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].CompanionReadError == "" {
		t.Fatalf("expected CompanionReadError on locked1, got %+v", files)
	}
}

func TestOmpReadHistorySpansSupplementalDeterminismAndRedaction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parentPath := filepath.Join(dir, "2026-11-20T10-00-00-000Z_sess-main.jsonl")
	subDir := filepath.Join(dir, "2026-11-20T10-00-00-000Z_sess-main")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subPath := filepath.Join(subDir, "0-explore.jsonl")

	hugeText := strings.Repeat("A", ompMaxToolText+1024)
	hugeDetails := `{"meta":"` + strings.Repeat("D", ompMaxRawDetails+512) + `","exitCode":0,"path":"/work/repo/a.go","written":true}`

	parentJSONL := strings.Join([]string{
		`{"type":"title","title":"Renamed Session","updatedAt":"2026-11-20T10:00:00.500Z"}`,
		`{"type":"session","version":3,"id":"sess-main","timestamp":"2026-11-20T10:00:00Z","cwd":"/work/repo","additionalDirectories":["/work/other"],"title":"Initial Title"}`,
		`{"type":"session_init","id":"init1","timestamp":"2026-11-20T10:00:00.100Z","runtime":{"version":"19.0.1"},"systemPrompt":"SECRET-SYSTEM-PROMPT","tools":["read","write","edit","bash"]}`,
		`{"type":"thinking_level_change","id":"th1","timestamp":"2026-11-20T10:00:00.200Z","thinkingLevel":"high"}`,
		`{"type":"model_change","id":"mc1","timestamp":"2026-11-20T10:00:00.300Z","model":"anthropic/claude-sonnet-4-6","role":"default"}`,
		`{"type":"title_change","id":"tc1","timestamp":"2026-11-20T10:00:00.400Z","title":"Renamed Session"}`,
		`{"type":"title_change","id":"tc2","timestamp":"2026-11-20T10:00:00.500Z","title":"Renamed Session"}`,
		// Turn 1: user message with text + image (base64 must not appear in OTLP output).
		`{"type":"message","id":"u1","timestamp":"2026-11-20T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"SECRET-USER-PROMPT"},{"type":"image","data":"SECRET-BASE64-PAYLOAD","mimeType":"image/png"}],"timestamp":1795168801000}}`,
		// Step 0 assistant response with thinking, text, and 3 tool calls: write (confirmed), write (internal agent:// URL), bash (failing exitCode).
		`{"type":"message","id":"a1","timestamp":"2026-11-20T10:00:03Z","message":{"role":"assistant","provider":"amazon-bedrock","model":"claude-sonnet-4-6","upstreamModel":"claude-sonnet-4-6-20261101","responseId":"resp_1","stopReason":"toolUse","duration":1500,"ttft":250,"timestamp":1795168803000,"usage":{"input":100,"output":50,"cacheRead":200,"cacheWrite":300,"reasoningTokens":12,"cost":{"input":0.01,"output":0.02,"cacheRead":0.001,"cacheWrite":0.002,"total":0.033}},"content":[{"type":"thinking","thinking":"SECRET-THINKING"},{"type":"text","text":"SECRET-STEP0-TEXT"},{"type":"toolCall","id":"call_w1","name":"write","arguments":{"path":"/work/repo/main.go","content":"SECRET-CODE"}},{"type":"toolCall","id":"call_w2","name":"write","arguments":{"path":"agent://scratch/note.md","content":"SECRET-NOTE"}},{"type":"toolCall","id":"call_b1","name":"bash","arguments":{"command":"cat /work/repo/main.go"}}]}}`,
		// Tool results: confirmed write, internal URL write (no files.touched), failing bash (exitCode 2), and an orphan toolResult (dropped).
		`{"type":"message","id":"tr1","timestamp":"2026-11-20T10:00:04Z","message":{"role":"toolResult","toolCallId":"call_w1","toolName":"write","content":[{"type":"text","text":` + mustJSON(hugeText) + `}],"details":` + hugeDetails + `,"isError":false,"timestamp":1795168804000}}`,
		`{"type":"message","id":"tr2","timestamp":"2026-11-20T10:00:04.200Z","message":{"role":"toolResult","toolCallId":"call_w2","toolName":"write","content":[{"type":"text","text":"ok"}],"isError":false,"timestamp":1795168804200}}`,
		`{"type":"message","id":"tr3","timestamp":"2026-11-20T10:00:04.500Z","message":{"role":"toolResult","toolCallId":"call_b1","toolName":"bash","content":[{"type":"text","text":"SECRET-STDERR"}],"details":{"exitCode":2},"isError":false,"timestamp":1795168804500}}`,
		`{"type":"message","id":"tr_orphan","timestamp":"2026-11-20T10:00:04.600Z","message":{"role":"toolResult","toolCallId":"unknown_call","toolName":"read","content":[{"type":"text","text":"orphan"}],"isError":false,"timestamp":1795168804600}}`,
		// Step 1 assistant response finishing the turn (must NOT repeat gen_ai.input.messages).
		`{"type":"message","id":"a2","timestamp":"2026-11-20T10:00:06Z","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5","stopReason":"stop","duration":800,"timestamp":1795168806000,"usage":{"input":10,"output":20},"content":[{"type":"text","text":"SECRET-FINAL-REPLY"}]}}`,
		// Auxiliary model_usage: oneshot + judgment.
		`{"type":"model_usage","id":"mu1","timestamp":"2026-11-20T10:00:07Z","purpose":"compaction_summary","provider":"anthropic","model":"claude-haiku-4-5","stopReason":"stop","duration":300,"ttft":50,"usage":{"input":50,"output":10,"cacheRead":5,"cacheWrite":0,"cost":{"total":0.001}}}`,
		`{"type":"model_usage","id":"mu2","timestamp":"2026-11-20T10:00:08Z","purpose":"ttsr","provider":"anthropic","model":"claude-haiku-4-5","stopReason":"error","duration":200,"usage":{"input":20,"output":5},"judgment":{"match":true}}`,
		// User bashExecution entry.
		`{"type":"message","id":"be1","timestamp":"2026-11-20T10:00:09Z","message":{"role":"bashExecution","command":"ls /work/extra","timestamp":1795168809000}}`,
	}, "\n") + "\n"

	subJSONL := strings.Join([]string{
		`{"type":"session","version":3,"id":"sess-sub","timestamp":"2026-11-20T10:00:05Z","cwd":"/work/repo","parentSession":` + mustJSON(parentPath) + `}`,
		`{"type":"session_init","id":"sinit","timestamp":"2026-11-20T10:00:05.050Z","agent":"explore","runtime":{"version":"19.0.1"}}`,
		`{"type":"message","id":"su1","timestamp":"2026-11-20T10:00:05.100Z","message":{"role":"user","content":"SECRET-SUBAGENT-PROMPT","timestamp":1795168805100}}`,
		`{"type":"message","id":"sa1","timestamp":"2026-11-20T10:00:05.500Z","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet-4-6","stopReason":"stop","duration":350,"timestamp":1795168805500,"usage":{"input":40,"output":15},"content":[{"type":"text","text":"SECRET-SUBAGENT-REPLY"}]}}`,
	}, "\n") + "\n"

	if err := os.WriteFile(parentPath, []byte(parentJSONL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subPath, []byte(subJSONL), 0o600); err != nil {
		t.Fatal(err)
	}

	readAll := func() []agents.HistorySegment {
		t.Helper()
		var segs []agents.HistorySegment
		err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
			SourceSystem: "omp",
			SessionID:    "sess-main",
			Path:         parentPath,
			ExtraPaths:   []string{subPath},
		}, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
			segs = append(segs, seg)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return segs
	}

	segs1 := readAll()
	segs2 := readAll()
	if len(segs1) != 2 {
		t.Fatalf("got %d segments, want 2 (parent + subagent)", len(segs1))
	}

	// Deterministic trace & span IDs across re-reads.
	if !proto.Equal(segs1[0].NativeTraces, segs2[0].NativeTraces) || !proto.Equal(segs1[1].NativeTraces, segs2[1].NativeTraces) {
		t.Fatalf("ReadHistory produced non-deterministic traces across identical runs")
	}

	// Inspect parent segment spans.
	parentSpans := collectSpans(segs1[0].NativeTraces)
	// Expected: chat(a1), tool(call_w1), tool(call_w2), tool(call_b1), chat(a2), oneshot(mu1), judgment(mu2) = 7 spans (orphan dropped).
	if len(parentSpans) != 7 {
		t.Fatalf("parent spans = %d, want 7", len(parentSpans))
	}
	chat0 := parentSpans[0]
	if got := spStr(chat0, "gen_ai.operation.name"); got != "chat" {
		t.Errorf("chat0 op = %q", got)
	}
	if got := spInt(chat0, "omp.gen_ai.agent.step.number"); got != 0 {
		t.Errorf("chat0 step = %d, want 0", got)
	}
	if got := spInt(chat0, semconv.GenAIUsageInputTokensKey); got != 600 {
		t.Errorf("chat0 input_tokens = %d, want 600 (100+200+300)", got)
	}
	if got := spInt(chat0, semconv.GenAIUsageCacheReadInputTokensKey); got != 200 {
		t.Errorf("chat0 cache_read = %d, want 200", got)
	}
	if got := spInt(chat0, "gen_ai.usage.cache_creation.input_tokens"); got != 300 {
		t.Errorf("chat0 cache_write = %d, want 300", got)
	}
	if got := spInt(chat0, semconv.GenAIUsageReasoningOutputTokensKey); got != 12 {
		t.Errorf("chat0 reasoning_tokens = %d, want 12", got)
	}
	if got := spStr(chat0, "gen_ai.provider.name"); got != "aws.bedrock" {
		t.Errorf("chat0 provider = %q, want aws.bedrock", got)
	}
	if got := spStr(chat0, semconv.GenAIResponseModelKey); got != "claude-sonnet-4-6-20261101" {
		t.Errorf("chat0 response.model = %q", got)
	}
	if got := spStr(chat0, "omp.gen_ai.request.reasoning.effort"); got != "high" {
		t.Errorf("chat0 reasoning.effort = %q, want high", got)
	}
	if got := spStrArr(chat0, semconv.GenAIResponseFinishReasonsKey); !slices.Equal(got, []string{"tool_calls"}) {
		t.Errorf("chat0 finish_reasons = %v, want [tool_calls]", got)
	}
	inMsg := spStr(chat0, "gen_ai.input.messages")
	if !strings.Contains(inMsg, "SECRET-USER-PROMPT") || !strings.Contains(inMsg, `"type":"blob"`) || strings.Contains(inMsg, "SECRET-BASE64-PAYLOAD") {
		t.Errorf("chat0 gen_ai.input.messages unexpected: %s", inMsg)
	}
	if spStr(chat0, "gen_ai.system_instructions") == "" {
		t.Errorf("chat0 missing gen_ai.system_instructions")
	}

	// Step 1 chat span must NOT re-attach gen_ai.input.messages or system_instructions.
	chat1 := parentSpans[4]
	if spInt(chat1, "omp.gen_ai.agent.step.number") != 1 {
		t.Errorf("chat1 step = %d, want 1", spInt(chat1, "omp.gen_ai.agent.step.number"))
	}
	if spStr(chat1, "gen_ai.provider.name") != "openai" {
		t.Errorf("chat1 provider = %q, want openai", spStr(chat1, "gen_ai.provider.name"))
	}
	if spStr(chat1, "gen_ai.input.messages") != "" || spStr(chat1, "gen_ai.system_instructions") != "" {
		t.Errorf("chat1 should not repeat input.messages or system_instructions")
	}
	if !slices.Equal(chat0.TraceId, chat1.TraceId) {
		t.Errorf("steps in one turn must share TraceId: %s vs %s", hex.EncodeToString(chat0.TraceId), hex.EncodeToString(chat1.TraceId))
	}

	// Tool spans: write (truncated result, huge details projected cleanly), bash (error status from exitCode 2).
	toolW1 := parentSpans[1]
	if res := spStr(toolW1, semconv.GenAIToolCallResultKey); len(res) > ompMaxToolText+4096 || strings.Contains(res, "DDDDDDDD") || !strings.Contains(res, "/work/repo/a.go") {
		t.Errorf("toolW1 huge result not capped/projected cleanly (len=%d)", len(res))
	}
	toolBash := parentSpans[3]
	if spStr(toolBash, "omp.gen_ai.tool.status") != "error" || toolBash.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR {
		t.Errorf("toolBash exitCode=2 should mark error status, got status=%q code=%v", spStr(toolBash, "omp.gen_ai.tool.status"), toolBash.GetStatus())
	}

	// Auxiliary spans: oneshot step=-1, judgment op=judgment + error status.
	auxOneshot := parentSpans[5]
	if spInt(auxOneshot, "omp.gen_ai.agent.step.number") != -1 || spStr(auxOneshot, "omp.gen_ai.oneshot.kind") != "compaction_summary" {
		t.Errorf("auxOneshot attrs unexpected: step=%d kind=%q", spInt(auxOneshot, "omp.gen_ai.agent.step.number"), spStr(auxOneshot, "omp.gen_ai.oneshot.kind"))
	}
	auxJudge := parentSpans[6]
	if spStr(auxJudge, "gen_ai.operation.name") != "judgment" || spStr(auxJudge, "omp.gen_ai.judgment.purpose") != "ttsr" || spStr(auxJudge, semconv.ErrorTypeKey) != "error" {
		t.Errorf("auxJudge attrs unexpected")
	}

	// Supplemental logs on parent: 1 dated title ("Renamed Session", deduped) + 1 files.touched (call_w1 only; call_w2 agent:// excluded).
	parentSupp := collectLogRecords(segs1[0].SupplementalLogs)
	var titles []string
	var touchedCalls []string
	for _, lr := range parentSupp {
		switch lr.GetEventName() {
		case semconv.TermaSessionTitleEvent:
			titles = append(titles, lrStr(lr, semconv.TermaSessionTitleKey))
		case semconv.TermaFilesTouchedEvent:
			touchedCalls = append(touchedCalls, lrStr(lr, semconv.GenAIToolCallIDKey))
		}
	}
	if !slices.Equal(titles, []string{"Renamed Session"}) {
		t.Errorf("titles = %v, want [Renamed Session]", titles)
	}
	if !slices.Equal(touchedCalls, []string{"call_w1"}) {
		t.Errorf("touchedCalls = %v, want [call_w1]", touchedCalls)
	}

	// Inspect subagent segment: resource parent ID, agent identity, supplemental session.start.
	subSeg := segs1[1]
	if !subSeg.Subagent || subSeg.SessionID != "sess-main" {
		t.Errorf("subSeg flags = Subagent:%v SessionID:%q", subSeg.Subagent, subSeg.SessionID)
	}
	subRS := subSeg.NativeTraces.ResourceSpans[0]
	if got := kvStr(subRS.Resource.Attributes, semconv.TermaSessionParentIDKey); got != "sess-main" {
		t.Errorf("subagent resource parent id = %q, want sess-main", got)
	}
	subSpans := collectSpans(subSeg.NativeTraces)
	if len(subSpans) != 1 || spStr(subSpans[0], "gen_ai.conversation.id") != "sess-sub" ||
		spStr(subSpans[0], "gen_ai.agent.id") != "0-explore" || spStr(subSpans[0], semconv.GenAIAgentNameKey) != "explore" {
		t.Errorf("subagent span identity unexpected: %+v", subSpans)
	}
	subSupp := collectLogRecords(subSeg.SupplementalLogs)
	if len(subSupp) != 1 || subSupp[0].GetEventName() != semconv.TermaSessionStartEvent ||
		lrStr(subSupp[0], semconv.TermaSessionParentIDKey) != "sess-main" || lrStr(subSupp[0], semconv.SessionIDKey) != "sess-sub" {
		t.Errorf("subagent supplemental start unexpected: %+v", subSupp)
	}

	// ContentFilter redaction check on generated OMP traces (both parent and subagent).
	filter := relay.NewContentFilter([]shape.Capturer{Agent{}})
	for idx, seg := range segs1 {
		cloned := proto.CloneOf(seg.NativeTraces)
		_, unclassified := filter.FilterTraces(cloned, false, false)
		if len(unclassified) != 0 {
			t.Errorf("segment %d had unclassified OMP keys: %v", idx, unclassified)
		}
		wire, err := proto.Marshal(cloned)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), "SECRET-") {
			t.Errorf("segment %d leaked withheld content after FilterTraces", idx)
		}
		rs := cloned.ResourceSpans[0]
		if !kvBool(rs.Resource.Attributes, semconv.TermaSessionHistoricalKey) {
			t.Errorf("segment %d lost resource %s", idx, semconv.TermaSessionHistoricalKey)
		}
		if idx == 1 && kvStr(rs.Resource.Attributes, semconv.TermaSessionParentIDKey) != "sess-main" {
			t.Errorf("subagent segment lost %s", semconv.TermaSessionParentIDKey)
		}
	}
}

func TestOmpHistoricalToolReachAndWindowsPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-11-20T10-00-00-000Z_reach1.jsonl")
	jsonl := strings.Join([]string{
		`{"type":"session","version":3,"id":"reach1","timestamp":"2026-11-20T10:00:00Z","cwd":"/work/repo","additionalDirectories":["/work/add-dir"]}`,
		`{"type":"message","id":"u1","timestamp":"2026-11-20T10:00:01Z","message":{"role":"user","content":"check files","timestamp":1795168801000}}`,
		`{"type":"message","id":"a1","timestamp":"2026-11-20T10:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","timestamp":1795168802000,"content":[` +
			`{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"/work/org-b/secret.txt"}},` +
			`{"type":"toolCall","id":"c2","name":"grep","arguments":{"pattern":"foo","path":"/work/repo/a.go;/work/org-b/b.go"}},` +
			`{"type":"toolCall","id":"c3","name":"find","arguments":{"pattern":"*.go","path":"/work/org-c"}},` +
			`{"type":"toolCall","id":"c4","name":"ls","arguments":{"path":"/work/org-d"}},` +
			`{"type":"toolCall","id":"c5","name":"bash","arguments":{"command":"cat /work/org-e/x.txt","cwd":"/work/repo"}},` +
			`{"type":"toolCall","id":"c6","name":"write","arguments":{"path":"agent://ignore/me.txt","content":"x"}},` +
			`{"type":"toolCall","id":"c7","name":"edit","arguments":{"path":"/work/repo/edit.go","diff":"*** Begin Patch\n*** Update File: /work/org-f/patched.go\n*** End Patch"}}` +
			`]}}`,
		`{"type":"message","id":"be1","timestamp":"2026-11-20T10:00:03Z","message":{"role":"bashExecution","command":"cat /work/org-g/y.txt","timestamp":1795168803000}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	var reaches []string
	err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "omp",
		SessionID:    "reach1",
		Path:         path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		for _, r := range seg.ToolReaches {
			for _, raw := range r.Paths {
				// Strip the hookrun.Alone marker prefix when asserting extracted paths.
				reaches = append(reaches, strings.TrimPrefix(raw, "\x00alone\x00"))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/work/add-dir",
		"/work/org-b/secret.txt",
		"/work/org-b/b.go",
		"/work/org-c",
		"/work/org-d",
		"/work/org-e/x.txt",
		"/work/org-f/patched.go",
		"/work/org-g/y.txt",
	} {
		if !slices.Contains(reaches, want) {
			t.Errorf("missing expected reach path %q in %v", want, reaches)
		}
	}
	for _, got := range reaches {
		if strings.HasPrefix(got, "agent://") {
			t.Errorf("internal agent:// URL should not appear in tool reaches: %q", got)
		}
	}

	// Windows drive-letter paths are preserved by nonURLPath and matched case-/slash-insensitively.
	if got := nonURLPath(`C:\Users\dev\repo\a.go`); got != `C:\Users\dev\repo\a.go` {
		t.Errorf("nonURLPath rejected Windows path: %q", got)
	}
	if got := nonURLPath("agent://worker/output.md"); got != "" {
		t.Errorf("nonURLPath kept internal URL: %q", got)
	}
	if !sameParentSessionPath(`C:\Users\Dev\Sessions\Parent\0-sub.jsonl`, `C:\Users\Dev\Sessions\Parent.jsonl`, `c:/users/dev/sessions/parent.jsonl`, true) {
		t.Errorf("sameParentSessionPath should match Windows paths case- and slash-insensitively")
	}
}

func TestOmpCouncilRegressionCases(t *testing.T) {
	// 1. Fork copied prefix is NOT re-emitted as spans, while copied tool reaches ARE retained;
	// new entries after header.timestamp emit spans normally; oversized write args are bounded;
	// updated systemPrompt re-emits; assistant without user starts at step 1.
	dir := t.TempDir()
	forkPath := filepath.Join(dir, "2026-11-21T10-00-00-000Z_forked.jsonl")
	hugeBody := strings.Repeat("Z", ompMaxToolArgs+8192)
	forkJSONL := strings.Join([]string{
		`{"type":"session","version":3,"id":"forked","timestamp":"2026-11-21T10:00:00Z","cwd":"/work/repo","parentSession":"parent-id"}`,
		`{"type":"session_init","id":"i0","timestamp":"2026-11-20T10:00:00Z","systemPrompt":"sys-v1"}`,
		// Copied prefix (2026-11-20 < header 2026-11-21): must NOT emit spans, MUST keep reach.
		`{"type":"message","id":"old_u","timestamp":"2026-11-20T10:00:01Z","message":{"role":"user","content":"old prompt","timestamp":1795168801000}}`,
		`{"type":"message","id":"old_a","timestamp":"2026-11-20T10:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","timestamp":1795168802000,"usage":{"input":99,"output":99},"content":[{"type":"toolCall","id":"old_c1","name":"read","arguments":{"path":"/work/org-b/copied.txt;/work/org-c/other.txt"}}]}}`,
		`{"type":"message","id":"old_tr","timestamp":"2026-11-20T10:00:03Z","message":{"role":"toolResult","toolCallId":"old_c1","toolName":"read","content":[{"type":"text","text":"ok"}],"details":{"displayReadTargets":["/work/org-d/target.txt"]},"isError":false,"timestamp":1795168803000}}`,
		`{"type":"model_usage","id":"old_mu","timestamp":"2026-11-20T10:00:04Z","purpose":"compaction","model":"claude-haiku-4-5","stopReason":"stop","usage":{"input":10,"output":5}}`,
		// Post-fork entries (2026-11-21 >= header): assistant without user starts at step 1, oversized write args bounded, bash ~ cwd & edit move reach extracted.
		`{"type":"message","id":"new_a1","timestamp":"2026-11-21T10:00:05Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":200,"timestamp":1795255205000,"usage":{"input":10,"output":20},"content":[{"type":"toolCall","id":"new_w1","name":"write","arguments":{"path":"/work/repo/big.txt","content":` + mustJSON(hugeBody) + `}},{"type":"toolCall","id":"new_b1","name":"bash","arguments":{"cwd":"~/org-e-repo","command":"ls"}},{"type":"toolCall","id":"new_e1","name":"edit","arguments":{"path":"/work/repo/a.go","move":"/work/org-f/b.go"}}]}}`,
		`{"type":"message","id":"new_tr1","timestamp":"2026-11-21T10:00:06Z","message":{"role":"toolResult","toolCallId":"new_w1","toolName":"write","content":[{"type":"text","text":"wrote"}],"isError":false,"timestamp":1795255206000}}`,
		// Updated system prompt on resume triggers a fresh gen_ai.system_instructions.
		`{"type":"session_init","id":"i1","timestamp":"2026-11-21T10:00:07Z","systemPrompt":"sys-v2"}`,
		`{"type":"message","id":"new_u2","timestamp":"2026-11-21T10:00:08Z","message":{"role":"user","content":"second turn","timestamp":1795255208000}}`,
		`{"type":"message","id":"new_a2","timestamp":"2026-11-21T10:00:09Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":100,"timestamp":1795255209000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"done"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(forkPath, []byte(forkJSONL), 0o600); err != nil {
		t.Fatal(err)
	}

	var segs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "omp", SessionID: "forked", Path: forkPath,
	}, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spans := collectSpans(segs[0].NativeTraces)
	// Only post-fork spans: new_a1, new_w1 toolResult, new_a2 (3 spans; old_a, old_tr, old_mu skipped).
	if len(spans) != 3 {
		t.Fatalf("fork spans = %d, want 3 (copied prefix must not be re-emitted)", len(spans))
	}
	if spInt(spans[0], "omp.gen_ai.agent.step.number") != 1 {
		t.Errorf("assistant without user step = %d, want 1 (no phantom prompt)", spInt(spans[0], "omp.gen_ai.agent.step.number"))
	}
	if len(spStr(spans[0], "gen_ai.output.messages")) > ompMaxToolArgs+4096 {
		t.Errorf("chat output.messages not bounded for oversized toolCall args")
	}
	if args := spStr(spans[1], semconv.GenAIToolCallArgumentsKey); len(args) > ompMaxToolArgs+1024 || !strings.Contains(args, `"/work/repo/big.txt"`) {
		t.Errorf("tool args not bounded/projected cleanly: len=%d", len(args))
	}
	if !strings.Contains(spStr(spans[2], "gen_ai.system_instructions"), "sys-v2") {
		t.Errorf("updated systemPrompt was not emitted on subsequent chat span")
	}
	var reaches []string
	for _, r := range segs[0].ToolReaches {
		for _, p := range r.Paths {
			reaches = append(reaches, strings.TrimPrefix(p, "\x00alone\x00"))
		}
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wantHomeRepo := filepath.Join(homeDir, "org-e-repo")
	for _, want := range []string{"/work/org-b/copied.txt", "/work/org-c/other.txt", "/work/org-d/target.txt", wantHomeRepo, "/work/org-f/b.go"} {
		if !slices.Contains(reaches, want) {
			t.Errorf("missing expected reach %q in %v", want, reaches)
		}
	}

	// 2. XDG_DATA_HOME discovery + SeenWatchFiles for stale copied subagents.
	xdg := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CONFIG_DIR", filepath.Join(t.TempDir(), ".omp"))
	t.Setenv("XDG_DATA_HOME", xdg)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	xdgProj := filepath.Join(xdg, "omp", "sessions", "--xdg-repo--")
	if err := os.MkdirAll(filepath.Join(xdgProj, "2026-11-23T10-00-00-000Z_xdg1"), 0o755); err != nil {
		t.Fatal(err)
	}
	xdgParent := filepath.Join(xdgProj, "2026-11-23T10-00-00-000Z_xdg1.jsonl")
	staleSub := filepath.Join(xdgProj, "2026-11-23T10-00-00-000Z_xdg1", "0-stale.jsonl")
	_ = os.WriteFile(xdgParent, []byte(`{"type":"session","version":3,"id":"xdg1","timestamp":"2026-11-23T10:00:00Z","cwd":"/work/repo"}`+"\n"), 0o600)
	_ = os.WriteFile(staleSub, []byte(`{"type":"session","version":3,"id":"sub-stale","timestamp":"2026-11-23T10:00:01Z","cwd":"/work/repo","parentSession":"/old/parent.jsonl"}`+"\n"), 0o600)
	_ = os.Chtimes(xdgParent, old, old)
	_ = os.Chtimes(staleSub, old, old)

	discovered, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 || discovered[0].SessionID != "xdg1" {
		t.Fatalf("XDG discovery = %+v, want [xdg1]", discovered)
	}
	if !slices.Contains(discovered[0].SeenWatchFiles, staleSub) {
		t.Errorf("stale copied subagent %q missing from SeenWatchFiles: %v", staleSub, discovered[0].SeenWatchFiles)
	}

	// Empty/incomplete companion file must NOT be added to SeenWatchFiles.
	emptySub := filepath.Join(xdgProj, "2026-11-23T10-00-00-000Z_xdg1", "1-incomplete.jsonl")
	_ = os.WriteFile(emptySub, []byte(""), 0o600)
	_ = os.Chtimes(emptySub, old, old)
	discovered2, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered2) != 1 || slices.Contains(discovered2[0].SeenWatchFiles, emptySub) {
		t.Errorf("incomplete empty subagent %q must NOT be in SeenWatchFiles", emptySub)
	}

	// 3. Promoted-model exception & assistantSpanTimes chronological ordering.
	promPath := filepath.Join(dir, "2026-11-22T10-00-00-000Z_prom1.jsonl")
	promJSONL := strings.Join([]string{
		`{"type":"session","version":3,"id":"prom1","timestamp":"2026-11-22T10:00:00Z","cwd":"/work/repo"}`,
		`{"type":"model_change","id":"mc","timestamp":"2026-11-22T10:00:00Z","model":"anthropic/model-a","role":"default"}`,
		`{"type":"message","id":"pu","timestamp":"2026-11-22T10:00:01Z","message":{"role":"user","content":"hi","timestamp":1795341601000}}`,
		`{"type":"message","id":"pa","timestamp":"2026-11-22T10:00:09Z","message":{"role":"assistant","model":"model-b","upstreamModel":"model-a","stopReason":"stop","duration":8000,"timestamp":1795341601000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"ok"}]}}`,
	}, "\n") + "\n"
	_ = os.WriteFile(promPath, []byte(promJSONL), 0o600)
	var promSegs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "omp", SessionID: "prom1", Path: promPath,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		promSegs = append(promSegs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	promSpans := collectSpans(promSegs[0].NativeTraces)
	if len(promSpans) != 1 {
		t.Fatalf("promSpans = %d, want 1", len(promSpans))
	}
	if spStr(promSpans[0], semconv.GenAIRequestModelKey) != "model-a" || spStr(promSpans[0], semconv.GenAIResponseModelKey) != "model-b" {
		t.Errorf("promoted model attrs: req=%q resp=%q, want model-a / model-b",
			spStr(promSpans[0], semconv.GenAIRequestModelKey), spStr(promSpans[0], semconv.GenAIResponseModelKey))
	}
	wantStart := uint64(time.UnixMilli(1795341601000).UnixNano())
	wantEnd := uint64(time.Date(2026, 11, 22, 10, 0, 9, 0, time.UTC).UnixNano())
	if promSpans[0].StartTimeUnixNano != wantStart || promSpans[0].EndTimeUnixNano != wantEnd {
		t.Errorf("span times = [%d, %d], want [%d, %d]", promSpans[0].StartTimeUnixNano, promSpans[0].EndTimeUnixNano, wantStart, wantEnd)
	}

	// 4. Local file:// decoding, OMP path aliases (@, :, quotes, \\?\), remote file:// fail-closed,
	// deleted whitespace-split selectors, and ompParentSessionID filename-first & non-regular-path safety.
	if got := nonURLPath("file:///work/org-b/plan%201.md"); got != "/work/org-b/plan 1.md" {
		t.Errorf("nonURLPath(file://) = %q", got)
	}
	if got := nonURLPath("file:///C:/Users/dev/a.go"); got != "C:/Users/dev/a.go" {
		t.Errorf("nonURLPath(windows file://) = %q", got)
	}
	if got, unk := resolveReachPath("@/work/org-b/plan.md"); unk || got != "/work/org-b/plan.md" {
		t.Errorf("resolveReachPath(@/...) = (%q, %v)", got, unk)
	}
	if got, unk := resolveReachPath(":/work/org-b/plan.md"); unk || got != "/work/org-b/plan.md" {
		t.Errorf("resolveReachPath(:/...) = (%q, %v)", got, unk)
	}
	if got, unk := resolveReachPath(`"/work/org-b/plan.md"`); unk || got != "/work/org-b/plan.md" {
		t.Errorf("resolveReachPath(quoted) = (%q, %v)", got, unk)
	}
	if got, unk := resolveReachPath("@my-file.txt"); unk || got != "@my-file.txt" {
		t.Errorf("resolveReachPath(@my-file.txt) = (%q, %v), want literal preserved", got, unk)
	}
	if _, unk := resolveReachPath("file://server/share/secret.txt"); !unk {
		t.Errorf("resolveReachPath(remote file://) should fail closed with unknown=true")
	}
	if got := stripPathSelector("/work/org-b/a.txt:1-2,5-6"); got != "/work/org-b/a.txt" {
		t.Errorf("stripPathSelector(multi-range) = %q", got)
	}
	if got := stripPathSelector("/work/org-b/a.txt:-60"); got != "/work/org-b/a.txt" {
		t.Errorf("stripPathSelector(tail) = %q", got)
	}
	if got := stripPathSelector("/work/org-b/a.txt:raw"); got != "/work/org-b/a.txt" {
		t.Errorf("stripPathSelector(raw) = %q", got)
	}
	wsDeleted := splitOmpDelimitedPaths("missing-a.txt:1-2 /work/org-b/missing-b.txt:1-2", dir)
	if !slices.Contains(wsDeleted, "/work/org-b/missing-b.txt:1-2") {
		t.Errorf("splitOmpDelimitedPaths(deleted) = %v, want /work/org-b/missing-b.txt:1-2", wsDeleted)
	}
	if got := ompParentSessionID("2026-11-20T10-00-00-000Z_parent1.jsonl"); got != "parent1" {
		t.Errorf("ompParentSessionID(bare filename) = %q, want parent1", got)
	}
	if got := ompParentSessionID(dir); !strings.HasPrefix(got, "missing-parent-") {
		t.Errorf("ompParentSessionID(directory) = %q, want missing-parent-*", got)
	}
}

func TestOmpRound6SecurityRegressions(t *testing.T) {
	dir := t.TempDir()

	// 1. ast_edit, lsp, security_scan, eval tool reach extraction + semicolon-split glob.pattern.
	astEditPaths := ompToolCallReachPaths("ast_edit", json.RawMessage(`{"ops":[{"pat":"a","out":"b"}],"paths":["/work/org-b/repo/a.ts","../../org-c/**/*.ts"]}`), "/work/org-a/repo")
	if !slices.Contains(astEditPaths, "/work/org-b/repo/a.ts") {
		t.Errorf("ast_edit missing /work/org-b/repo/a.ts: %v", astEditPaths)
	}
	if !slices.Contains(astEditPaths, "/work/org-c") {
		t.Errorf("ast_edit glob path missing /work/org-c: %v", astEditPaths)
	}
	globPaths := ompToolCallReachPaths("glob", json.RawMessage(`{"pattern":"*.go;/work/org-b/*.ts"}`), "/work/org-a/repo")
	if !slices.Contains(globPaths, "/work/org-b") {
		t.Errorf("glob semicolon pattern missing /work/org-b: %v", globPaths)
	}
	lspPaths := ompToolCallReachPaths("lsp", json.RawMessage(`{"action":"rename_file","file":"/work/org-a/a.go","new_name":"/work/org-b/b.go"}`), "/work/org-a/repo")
	if !slices.ContainsFunc(lspPaths, func(s string) bool { return strings.HasSuffix(s, "/work/org-b/b.go") }) {
		t.Errorf("lsp rename_file missing destination reach: %v", lspPaths)
	}
	scanPaths := ompToolCallReachPaths("security_scan", json.RawMessage(`{"include_paths":["/work/org-b/src"]}`), "/work/org-a/repo")
	if !slices.ContainsFunc(scanPaths, func(s string) bool { return strings.HasSuffix(s, "/work/org-b/src") }) {
		t.Errorf("security_scan missing include_paths reach: %v", scanPaths)
	}
	evalPaths := ompToolCallReachPaths("eval", json.RawMessage(`{"language":"py","code":"print(1)"}`), "/work/org-a/repo")
	if !slices.ContainsFunc(evalPaths, func(s string) bool { return strings.HasPrefix(s, "\x00unknown\x00") }) {
		t.Errorf("eval should fail closed with UnknownReach: %v", evalPaths)
	}

	// 2. @../, @./, @.., and quotes after @ / :.
	for _, tc := range []struct{ in, want string }{
		{"@../org-b/secret.txt", "../org-b/secret.txt"},
		{"@./../org-b/secret.txt", "./../org-b/secret.txt"},
		{"@..", ".."},
		{`@"/work/org-b/secret.txt"`, "/work/org-b/secret.txt"},
		{`:"/work/org-b/secret.txt"`, "/work/org-b/secret.txt"},
		{"@scope/pkg", "@scope/pkg"},
	} {
		if got, unk := resolveReachPath(tc.in); unk || got != tc.want {
			t.Errorf("resolveReachPath(%q) = (%q, %v), want (%q, false)", tc.in, got, unk, tc.want)
		}
	}

	// 3. Windows drive / UNC paths fail closed on non-Windows hosts instead of joining under cwd.
	if runtime.GOOS != "windows" {
		t.Setenv("WSL_DISTRO_NAME", "")
		t.Setenv("WSL_INTEROP", "")
		for _, winPath := range []string{`C:/work/org-b/file.ts`, `C:\work\org-b\file.ts`, `\\server\share\file.ts`} {
			if _, unk := resolveReachPath(winPath); !unk {
				t.Errorf("resolveReachPath(%q) on %s should fail closed with unknown=true", winPath, runtime.GOOS)
			}
		}
	}

	// 4. Spaced enclosing directory keeps deleted absolute path whole while still splitting multi-target calls.
	spacedRepo := filepath.Join(dir, "My Projects", "repo-a")
	if err := os.MkdirAll(spacedRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	deletedInSpaced := filepath.Join(spacedRepo, "deleted.go")
	if got := splitOmpDelimitedPaths(deletedInSpaced, spacedRepo); !slices.Equal(got, []string{deletedInSpaced}) {
		t.Errorf("splitOmpDelimitedPaths(deleted in spaced dir) = %v, want [%q]", got, deletedInSpaced)
	}
	if got := splitOmpDelimitedPaths(deletedInSpaced+" /work/org-b/b.go", spacedRepo); !slices.Contains(got, "/work/org-b/b.go") {
		t.Errorf("splitOmpDelimitedPaths(spaced + orgB) = %v, want /work/org-b/b.go", got)
	}
	if got := splitOmpDelimitedPaths("local.txt ~", spacedRepo); !slices.Contains(got, "~") {
		t.Errorf("splitOmpDelimitedPaths(local.txt ~) = %v, want ~", got)
	}

	// 5. Oversized (> 1 MiB) session header returns an error from peekOmpSessionHeader and fails closed in DiscoverHistory.
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	projDir := filepath.Join(agentDir, "sessions", "--work-repo--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bigPath := filepath.Join(projDir, "2026-11-23T10-00-00-000Z_s-big-hdr.jsonl")
	bigHeader := `{"type":"session","version":3,"id":"s-big-hdr","timestamp":"2026-11-23T10:00:00Z","cwd":"/work/repo","parentSession":"parent-x","pad":"` + strings.Repeat("A", (1<<20)+64) + `"}` + "\n"
	if err := os.WriteFile(bigPath, []byte(bigHeader), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := peekOmpSessionHeader(bigPath); err == nil {
		t.Errorf("peekOmpSessionHeader should return error when 1 MiB cap is exhausted without session header")
	}

	// 6. Subagent recursion past depth 8 fails closed with CompanionReadError when a depth-9 .jsonl exists,
	// while a depth-8 leaf without deeper transcripts stays clean.
	writeChain := func(rootName, rootID string, totalDepth int) {
		parentPath := filepath.Join(projDir, rootName+".jsonl")
		if err := os.WriteFile(parentPath, []byte(`{"type":"session","version":3,"id":"`+rootID+`","timestamp":"2026-11-23T10:00:00Z","cwd":"/work/repo"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		curDir := filepath.Join(projDir, rootName)
		curParent := parentPath
		for d := 0; d < totalDepth; d++ {
			if err := os.MkdirAll(curDir, 0o755); err != nil {
				t.Fatal(err)
			}
			subStem := "0-sub"
			subPath := filepath.Join(curDir, subStem+".jsonl")
			hdr := `{"type":"session","version":3,"id":"` + rootID + `-d` + string(rune('0'+d)) + `","timestamp":"2026-11-23T10:00:01Z","cwd":"/work/repo","parentSession":` + mustJSON(curParent) + `}` + "\n"
			if err := os.WriteFile(subPath, []byte(hdr), 0o600); err != nil {
				t.Fatal(err)
			}
			curParent = subPath
			curDir = filepath.Join(curDir, subStem)
		}
	}
	writeChain("2026-11-23T10-00-00-000Z_s-depth8", "s-depth8", 9)  // depth 0..8 leaves (clean)
	writeChain("2026-11-23T10-00-00-000Z_s-depth9", "s-depth9", 10) // has depth 9 beyond cap (fail closed)

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]agents.HistoryFile{}
	for _, f := range files {
		byID[f.SessionID] = f
	}
	if byID["s-big-hdr"].CompanionReadError == "" {
		t.Errorf("oversized header session should set CompanionReadError")
	}
	if byID["s-depth8"].CompanionReadError != "" {
		t.Errorf("depth-8 leaf session should not set CompanionReadError, got %q", byID["s-depth8"].CompanionReadError)
	}
	if byID["s-depth9"].CompanionReadError == "" {
		t.Errorf("session with subagent past depth 8 should set CompanionReadError")
	}
}

func TestOmpChangesJournalSidecar(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-2 * time.Hour)

	projDir := filepath.Join(agentDir, "sessions", "--work-repo--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAged := func(rel, content string, mt time.Time) string {
		full := filepath.Join(projDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, mt, mt); err != nil {
			t.Fatal(err)
		}
		return full
	}

	// 1. Normal session with <stem>/changes.jsonl (no session header) + orphan subdir holding only changes.jsonl.
	writeAged("2026-11-23T10-00-00-000Z_s-changes.jsonl",
		`{"type":"session","version":3,"id":"s-changes","timestamp":"2026-11-23T10:00:00Z","cwd":"/work/repo"}`+"\n", oldTime)
	changesPath := writeAged(filepath.Join("2026-11-23T10-00-00-000Z_s-changes", "changes.jsonl"),
		"{\"type\":\"change\",\"path\":\"a.go\"}\n{\"type\":\"commit\",\"sha\":\"abc\"}\n", now)
	writeAged(filepath.Join("2026-11-23T10-00-00-000Z_s-changes", "0-deleted-sub", "changes.jsonl"),
		"{\"type\":\"change\",\"path\":\"b.go\"}\n", oldTime)

	// 2. Session with oversized (> 1 MiB first line) changes.jsonl must not trigger CompanionReadError.
	writeAged("2026-11-23T09-00-00-000Z_s-big-changes.jsonl",
		`{"type":"session","version":3,"id":"s-big-changes","timestamp":"2026-11-23T09:00:00Z","cwd":"/work/repo"}`+"\n", oldTime)
	bigChangesPath := writeAged(filepath.Join("2026-11-23T09-00-00-000Z_s-big-changes", "changes.jsonl"),
		`{"type":"change","diff":"`+strings.Repeat("X", (1<<20)+64)+`"}`+"\n", oldTime)

	// 3. Session with freshly-started incomplete subagent transcript (not changes.jsonl) must NOT be exempted.
	writeAged("2026-11-23T08-00-00-000Z_s-starting.jsonl",
		`{"type":"session","version":3,"id":"s-starting","timestamp":"2026-11-23T08:00:00Z","cwd":"/work/repo"}`+"\n", oldTime)
	startingSub := writeAged(filepath.Join("2026-11-23T08-00-00-000Z_s-starting", "1-starting.jsonl"),
		`{"type":"session","id":`, oldTime)

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]agents.HistoryFile{}
	for _, f := range files {
		byID[f.SessionID] = f
	}

	sc := byID["s-changes"]
	if !slices.Contains(sc.SeenWatchFiles, changesPath) {
		t.Errorf("s-changes SeenWatchFiles = %v, want %q", sc.SeenWatchFiles, changesPath)
	}
	if len(sc.ExtraPaths) != 0 {
		t.Errorf("s-changes ExtraPaths = %v, want empty", sc.ExtraPaths)
	}
	if sc.CompanionReadError != "" {
		t.Errorf("s-changes CompanionReadError = %q, want empty", sc.CompanionReadError)
	}
	if sc.Active {
		t.Errorf("s-changes Active = true; changes.jsonl mtime should not mark session active")
	}

	sb := byID["s-big-changes"]
	if !slices.Contains(sb.SeenWatchFiles, bigChangesPath) {
		t.Errorf("s-big-changes SeenWatchFiles = %v, want %q", sb.SeenWatchFiles, bigChangesPath)
	}
	if sb.CompanionReadError != "" {
		t.Errorf("s-big-changes CompanionReadError = %q, want empty", sb.CompanionReadError)
	}

	ss := byID["s-starting"]
	if slices.Contains(ss.SeenWatchFiles, startingSub) || slices.Contains(ss.ExtraPaths, startingSub) {
		t.Errorf("incomplete subagent %q must remain un-exempted: seen=%v extra=%v", startingSub, ss.SeenWatchFiles, ss.ExtraPaths)
	}
}

func TestOmpEvalFailClosedAndEmbeddedSearchGlobs(t *testing.T) {
	cwd := t.TempDir()
	hasUnknown := func(paths []string) bool {
		return slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(p, "\x00unknown\x00") })
	}

	// 1. eval executes arbitrary Python/JS code and must always emit UnknownReach.
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"language":"py","code":"print(open('/work/org-b/secret.txt').read())"}`),
		json.RawMessage(`{"language":"js","code":"console.log(1)"}`),
		nil,
		json.RawMessage(`{invalid`),
	} {
		got := ompToolCallReachPaths("eval", raw, cwd)
		if !hasUnknown(got) {
			t.Errorf("eval(%s) = %v, want UnknownReach", string(raw), got)
		}
	}

	// 2. Embedded globs in search path/paths split to (baseDir, embeddedGlob) matching OMP's parseSearchPath.
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"x","path":"**/*.txt"}`), cwd); !slices.Contains(got, cwd) {
		t.Errorf("grep path=**/*.txt = %v, want cwd %q so ExpandReachHistorical walks nested checkouts", got, cwd)
	}
	wantSafe := filepath.Join(cwd, "safe")
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"x","path":"safe/*.txt"}`), cwd); !slices.Equal(got, []string{wantSafe}) {
		t.Errorf("grep path=safe/*.txt = %v, want [%q]", got, wantSafe)
	}
	wantSub := filepath.Join(cwd, "sub")
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"x","path":"sub/**/*.txt"}`), cwd); !slices.Equal(got, []string{wantSub}) {
		t.Errorf("grep path=sub/**/*.txt = %v, want [%q]", got, wantSub)
	}
	wantRelB := filepath.Clean(filepath.Join(cwd, "..", "org-b"))
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"x","path":"../org-b/*.txt"}`), cwd); !slices.Contains(got, wantRelB) {
		t.Errorf("grep path=../org-b/*.txt = %v, want %q", got, wantRelB)
	}
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"x","path":"/abs/root/**/*.txt"}`), cwd); !slices.Contains(got, filepath.FromSlash("/abs/root")) {
		t.Errorf("grep path=/abs/root/**/*.txt = %v, want /abs/root", got)
	}
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"x","path":"{safe,../org-b}/*.txt"}`), cwd); !slices.Contains(got, wantRelB) {
		t.Errorf("grep path={safe,../org-b}/*.txt = %v, want %q", got, wantRelB)
	}
	for _, failClosedPath := range []string{"/*/*.txt", "/**/*.txt", "{a,{b,c}}/*.txt"} {
		raw := json.RawMessage(`{"pattern":"x","path":` + mustJSON(failClosedPath) + `}`)
		if got := ompToolCallReachPaths("grep", raw, cwd); !hasUnknown(got) {
			t.Errorf("grep path=%q = %v, want UnknownReach", failClosedPath, got)
		}
	}

	// Literal directory/file containing glob metacharacters (e.g. apps/[id]/page.tsx) stays literal.
	litFile := filepath.Join(cwd, "apps", "[id]", "page.tsx")
	if err := os.MkdirAll(filepath.Dir(litFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(litFile, []byte("export default 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"x","path":"apps/[id]/page.tsx"}`), cwd); !slices.Equal(got, []string{"apps/[id]/page.tsx"}) {
		t.Errorf("literal existing path with brackets = %v, want [apps/[id]/page.tsx]", got)
	}

	// 3. Round 7 blockers & should: debug fail-closed, ssh:// fail-closed, backslash glob paths
	// on Linux/WSL, Changes.jsonl vs changes.jsonl content check, and multi-level spaced dirs.
	if got := ompToolCallReachPaths("debug", json.RawMessage(`{"action":"launch","cwd":"/work/org-b","program":"app"}`), cwd); !hasUnknown(got) {
		t.Errorf("debug should fail closed with UnknownReach: %v", got)
	}
	if _, unk := resolveReachPath("ssh://client-host/work/repo/README.md"); !unk {
		t.Errorf("ssh:// URI should fail closed with unknown=true")
	}
	if _, unk := resolveReachPath("vault://notes/secret.md"); !unk {
		t.Errorf("vault:// URI should fail closed with unknown=true")
	}
	if got, unk := resolveReachPath("agent://worker/out.md"); unk || got != "" {
		t.Errorf("agent:// URI should be safely ignored, got (%q, %v)", got, unk)
	}
	if got := ompToolCallReachPaths("glob", json.RawMessage(`{"path":"..\\org-b\\*.go"}`), cwd); !slices.Contains(got, wantRelB) {
		t.Errorf("backslash glob path ..\\org-b\\*.go = %v, want %q", got, wantRelB)
	}
	// Multi-level glob and deleted subdir under a spaced parent directory stay unsplit.
	spacedRoot := filepath.Join(cwd, "My Projects", "org-a", "repo")
	if err := os.MkdirAll(filepath.Join(spacedRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	spacedGlob := filepath.Join(spacedRoot, "src", "**", "*.go")
	if got := splitOmpDelimitedPaths(spacedGlob, spacedRoot); !slices.Equal(got, []string{spacedGlob}) {
		t.Errorf("splitOmpDelimitedPaths(spaced multi-level glob) = %v, want [%q]", got, spacedGlob)
	}
	spacedDeletedSub := filepath.Join(spacedRoot, "old-dir", "file.go")
	if got := splitOmpDelimitedPaths(spacedDeletedSub, spacedRoot); !slices.Equal(got, []string{spacedDeletedSub}) {
		t.Errorf("splitOmpDelimitedPaths(spaced deleted subdir) = %v, want [%q]", got, spacedDeletedSub)
	}
	// Changes.jsonl (task subagent named Changes) is NOT exempted even alongside changes.jsonl.
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		subDir := filepath.Join(cwd, "sub-case")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		capSub := filepath.Join(subDir, "Changes.jsonl")
		if err := os.WriteFile(capSub, []byte(`{"type":"session","id":"s-cap","timestamp":"2026-11-23T10:00:00Z","cwd":"/work/repo"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if isOmpChangesJournal(capSub, "Changes.jsonl") {
			t.Errorf("Changes.jsonl subagent should not be treated as changes.jsonl journal")
		}
	}
	sessionNamedChanges := filepath.Join(cwd, "changes.jsonl")
	if err := os.WriteFile(sessionNamedChanges, []byte(`{"type":"session","id":"s-lower","timestamp":"2026-11-23T10:00:00Z","cwd":"/work/repo"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isOmpChangesJournal(sessionNamedChanges, "changes.jsonl") {
		t.Errorf("session transcript starting with type=session must never be treated as changes.jsonl journal")
	}
	emptyChanges := filepath.Join(cwd, "empty-dir", "changes.jsonl")
	if err := os.MkdirAll(filepath.Dir(emptyChanges), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emptyChanges, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if isOmpChangesJournal(emptyChanges, "changes.jsonl") {
		t.Errorf("empty 0-byte changes.jsonl must not be exempted")
	}

	// 4. Round 8 fixes: !negation path fallback to cwd, generate_image input[].path,
	// backslash normalization in glob.pattern, browser/computer/tts, and /tan bare parentSession ID.
	if got := ompToolCallReachPaths("grep", json.RawMessage(`{"pattern":"secret","path":"!vendor"}`), cwd); !slices.Contains(got, cwd) {
		t.Errorf("grep path=!vendor = %v, want fallback to cwd %q", got, cwd)
	}
	if got := ompToolCallReachPaths("glob", json.RawMessage(`{"pattern":"..\\org-b\\*.txt"}`), cwd); !slices.Contains(got, wantRelB) {
		t.Errorf("glob pattern=..\\org-b\\*.txt = %v, want %q", got, wantRelB)
	}
	if got := ompToolCallReachPaths("generate_image", json.RawMessage(`{"subject":"edit","input":[{"path":"/work/org-b/ref.png"}]}`), cwd); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, "/work/org-b/ref.png") }) {
		t.Errorf("generate_image input[].path = %v, want /work/org-b/ref.png", got)
	}
	if got := ompToolCallReachPaths("tts", json.RawMessage(`{"output_path":"/work/org-b/out.mp3"}`), cwd); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, "/work/org-b/out.mp3") }) {
		t.Errorf("tts output_path = %v, want /work/org-b/out.mp3", got)
	}
	for _, tool := range []string{"browser", "computer", "run_code", "python"} {
		if got := ompToolCallReachPaths(tool, json.RawMessage(`{}`), cwd); !hasUnknown(got) {
			t.Errorf("%s should fail closed with UnknownReach, got %v", tool, got)
		}
	}
	if !matchesParentSession(filepath.Join(cwd, "p", "tan-1.jsonl"), filepath.Join(cwd, "p.jsonl"), "parent-123", "parent-123", false) {
		t.Errorf("matchesParentSession should match bare parent session ID for /tan children")
	}

	// 5. Round 9 fixes: top-level /fork with bare parent ID stays non-subagent (emits title, no session.start),
	// and read on a directory emits non-Alone directory reach so nested checkouts are walked.
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	projDir := filepath.Join(agentDir, "sessions", "--work-repo--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	forkFile := filepath.Join(projDir, "2026-11-23T11-00-00-000Z_fork-bare.jsonl")
	forkData := strings.Join([]string{
		`{"type":"session","version":3,"id":"fork-bare","timestamp":"2026-11-23T11:00:00Z","cwd":"/work/repo","parentSession":"parent-bare","title":"Forked Title"}`,
		`{"type":"message","id":"u1","timestamp":"2026-11-23T11:00:01Z","message":{"role":"user","content":"hi","timestamp":1795431601000}}`,
		`{"type":"message","id":"a1","timestamp":"2026-11-23T11:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":100,"timestamp":1795431601900,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"hello"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(forkFile, []byte(forkData), 0o600); err != nil {
		t.Fatal(err)
	}
	disc, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var forkHF agents.HistoryFile
	for _, f := range disc {
		if f.SessionID == "fork-bare" {
			forkHF = f
		}
	}
	if !forkHF.OneWayParent || forkHF.ParentSessionID != "parent-bare" {
		t.Fatalf("discovered fork = %+v, want OneWayParent=true ParentSessionID=parent-bare", forkHF)
	}
	var gotTitle, gotStart bool
	if err := (Agent{}).ReadHistory(context.Background(), forkHF, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		if seg.Subagent {
			t.Errorf("top-level /fork emitted Subagent=true")
		}
		for _, lr := range collectLogRecords(seg.SupplementalLogs) {
			if lr.GetEventName() == semconv.TermaSessionTitleEvent {
				gotTitle = true
			}
			if lr.GetEventName() == semconv.TermaSessionStartEvent {
				gotStart = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !gotTitle || gotStart {
		t.Errorf("top-level /fork supplemental logs: gotTitle=%v gotStart=%v, want true/false", gotTitle, gotStart)
	}

	readDirCall := ompToolCallReachPaths("read", json.RawMessage(`{"path":"."}`), cwd)
	if !slices.Contains(readDirCall, ".") {
		t.Errorf("read on directory '.' should emit non-Alone directory reach '.', got %v", readDirCall)
	}
	readDirRes := ompToolResultReachPaths("read", &ompToolDetails{ResolvedPath: cwd, IsDirectory: true}, cwd)
	if !slices.Contains(readDirRes, cwd) {
		t.Errorf("read result with isDirectory=true should emit non-Alone reach %q, got %v", cwd, readDirRes)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mapKeys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func collectSpans(msg *tracepb.TracesData) []*tracepb.Span {
	var out []*tracepb.Span
	for _, rs := range msg.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			out = append(out, ss.GetSpans()...)
		}
	}
	return out
}

func collectLogRecords(msg *logspb.LogsData) []*logspb.LogRecord {
	var out []*logspb.LogRecord
	for _, rl := range msg.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			out = append(out, sl.GetLogRecords()...)
		}
	}
	return out
}

func kvStr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func kvBool(attrs []*commonpb.KeyValue, key string) bool {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetBoolValue()
		}
	}
	return false
}

func spStr(sp *tracepb.Span, key string) string { return kvStr(sp.GetAttributes(), key) }
func lrStr(lr *logspb.LogRecord, key string) string {
	return kvStr(lr.GetAttributes(), key)
}

func spInt(sp *tracepb.Span, key string) int64 {
	for _, kv := range sp.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetIntValue()
		}
	}
	return 0
}

func spStrArr(sp *tracepb.Span, key string) []string {
	for _, kv := range sp.GetAttributes() {
		if kv.GetKey() == key {
			vals := kv.GetValue().GetArrayValue().GetValues()
			out := make([]string, 0, len(vals))
			for _, v := range vals {
				out = append(out, v.GetStringValue())
			}
			return out
		}
	}
	return nil
}
