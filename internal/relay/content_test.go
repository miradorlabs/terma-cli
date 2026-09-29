package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func withheldAll(t *testing.T, sig signal, rec any, p ContentPolicy) string {
	t.Helper()
	out, _ := withholdRecord(sig, mustJSON(t, rec), p)
	return string(out)
}

var withholdEverything = ContentPolicy{}

// Each harness's content, as it exports it, is gone from a project that withholds it —
// in the harness's own shape, so the backend parses what it always has.
func TestWithholdRemovesEveryHarnessesContent(t *testing.T) {
	// Claude Code: the prompt is blanked with Claude's marker; tool content removed.
	claude := logRecord("user_prompt", "", strAttr("session.id", sessionA), strAttr("prompt", "SECRET-PROMPT"),
		strAttr("tool_parameters", "SECRET-TOOL"), strAttr("full_command", "SECRET-CMD"))
	got := withheldAll(t, sigLogs, claude, withholdEverything)
	for _, leak := range []string{"SECRET-PROMPT", "SECRET-TOOL", "SECRET-CMD", "tool_parameters"} {
		if strings.Contains(got, leak) {
			t.Errorf("Claude log kept %s: %s", leak, got)
		}
	}
	if !strings.Contains(got, claudeRedacted) || !strings.Contains(got, sessionA) {
		t.Errorf("Claude log: want the prompt blanked with %s and the session kept: %s", claudeRedacted, got)
	}

	// Codex: its own marker, recognised by its conversation id.
	codex := logRecord("codex.user_prompt", "", strAttr("conversation.id", codexID), strAttr("prompt", "SECRET-PROMPT"))
	if got := withheldAll(t, sigLogs, codex, withholdEverything); !strings.Contains(got, codexRedacted) || strings.Contains(got, "SECRET") {
		t.Errorf("Codex log: %s", got)
	}

	// OpenCode: the prompt is the log body; the reply a GenAI attribute.
	opencode := logRecord("opencode.user_prompt", "", strAttr("session.id", sessionA), strAttr("gen_ai.completion", "SECRET-REPLY"))
	opencode["body"] = map[string]any{"stringValue": "SECRET-BODY"}
	if got := withheldAll(t, sigLogs, opencode, withholdEverything); strings.Contains(got, "SECRET") {
		t.Errorf("OpenCode log kept content: %s", got)
	}

	// Claude's tool span: its tool.output event holds the command and a file's content.
	span := map[string]any{"name": "claude_code.tool", "traceId": "t", "attributes": []any{strAttr("session.id", sessionA)},
		"events": []any{
			map[string]any{"name": "tool.output", "attributes": []any{strAttr("bash_command", "SECRET-BASH"), strAttr("content", "SECRET-FILE")}},
			map[string]any{"name": "tool.decision", "attributes": []any{strAttr("decision", "accept")}},
		}}
	got = withheldAll(t, sigTraces, span, withholdEverything)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "tool.decision") {
		t.Errorf("tool span: want tool.output gone and other events kept: %s", got)
	}

	// Prompts allowed, tool content withheld: only the tool content goes.
	got = withheldAll(t, sigLogs, claude, ContentPolicy{Prompts: true})
	if !strings.Contains(got, "SECRET-PROMPT") || strings.Contains(got, "SECRET-TOOL") {
		t.Errorf("prompts on, tools off: %s", got)
	}
}

func TestAllowAllLeavesRecordsByteForByte(t *testing.T) {
	raw := mustJSON(t, logRecord("user_prompt", "", strAttr("prompt", "kept")))
	out, changed := withholdRecord(sigLogs, raw, ContentPolicy{Prompts: true, ToolContent: true})
	if changed || string(out) != string(raw) {
		t.Fatalf("allow-all changed the record: %s", out)
	}
}

// A machine with no recorded policy sends no content; an unreadable policy file is read
// as withholding (it might be the one that does).
func TestContentPolicyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if p, ok := loadContentPolicy(dir, "proj"); ok || p.Prompts || p.ToolContent {
		t.Fatalf("no policy: %+v %v, want withhold everything", p, ok)
	}
	h := &testRelay{t: t, dir: dir}
	h.setPolicy(MachineRoute, ContentPolicy{Prompts: true, ToolContent: true})
	if p, _ := loadContentPolicy(dir, "proj"); !p.allowsAll() {
		t.Fatalf("a project with none of its own takes the machine default: %+v", p)
	}
	h.setPolicy("proj", ContentPolicy{Prompts: false, ToolContent: true})
	if p, _ := loadContentPolicy(dir, "proj"); p.Prompts || !p.ToolContent {
		t.Fatalf("a project's own policy wins: %+v", p)
	}
	if err := os.WriteFile(filepath.Join(dir, policyDir, "proj.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, ok := loadContentPolicy(dir, "proj"); !ok || p.Prompts || p.ToolContent {
		t.Fatalf("an unreadable policy: %+v %v, want withhold everything", p, ok)
	}
}

// Everything upstream received is scanned for planted markers: a project that
// withholds content receives none of them, from any harness, in any field.
func TestRelayWithholdsContentPerProject(t *testing.T) {
	h := newTestRelay(t)
	h.bindings[repoPath("open")] = "proj-open"
	h.bindings[repoPath("closed")] = "proj-closed"
	h.keys["proj-open"] = "Bearer key-open"
	h.keys["proj-closed"] = "Bearer key-closed"
	h.recordSession(sessionA, repoPath("open"))
	h.recordSession(sessionB, repoPath("closed"))
	h.setPolicy("proj-open", ContentPolicy{Prompts: true, ToolContent: true})
	h.setPolicy("proj-closed", ContentPolicy{})
	h.start()

	marked := func(session string) []map[string]any {
		return []map[string]any{
			logRecord("user_prompt", "", strAttr("session.id", session), strAttr("prompt", "MARK-PROMPT-"+session)),
			logRecord("tool_result", "", strAttr("session.id", session), strAttr("tool_parameters", "MARK-TOOL-"+session)),
		}
	}
	h.post(sigLogs, logsBody(t, append(marked(sessionA), marked(sessionB)...)...))
	spans, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{"scopeSpans": []any{map[string]any{"spans": []any{
		map[string]any{"name": "claude_code.tool", "traceId": "tt", "attributes": []any{strAttr("session.id", sessionB)},
			"events": []any{map[string]any{"name": "tool.output", "attributes": []any{strAttr("bash_command", "MARK-BASH-"+sessionB)}}}},
	}}}}}})
	h.post(sigTraces, spans)

	eventually(t, "both projects delivered", func() bool {
		return len(h.gw.received("Bearer key-open")) == 2 && len(h.gw.received("Bearer key-closed")) == 3
	})
	closed := h.gw.bodiesFor("Bearer key-closed")
	if strings.Contains(closed, "MARK-") {
		t.Fatalf("a project that withholds content received it:\n%s", closed)
	}
	if !strings.Contains(closed, sessionB) || !strings.Contains(closed, claudeRedacted) {
		t.Fatalf("the withholding project lost its records' shape:\n%s", closed)
	}
	if open := h.gw.bodiesFor("Bearer key-open"); !strings.Contains(open, "MARK-PROMPT-"+sessionA) || !strings.Contains(open, "MARK-TOOL-"+sessionA) {
		t.Fatalf("a project that allows content lost it:\n%s", open)
	}
	eventually(t, "withheld records counted", func() bool {
		hl, err := probeAt(h)
		return err == nil && hl.Counters.Withheld == 3
	})
}

// A long Codex turn: its child spans name no session and arrive long before anything in
// their trace does. They wait past the session hold, and reach the session's project.
func TestTraceHoldOutlastsTheSessionHold(t *testing.T) {
	dir := t.TempDir()
	start := time.Unix(1790700000, 0)
	clock := start
	rt, routed := newTestRouter(t, dir, 30*time.Second, &clock, map[string]string{repoPath("codex"): "proj-codex"})
	rt.traceHold = 30 * time.Minute
	h := &testRelay{t: t, dir: dir}
	h.setPolicy(MachineRoute, ContentPolicy{Prompts: true, ToolContent: true})
	if err := writeEntry(filepath.Join(dir, inboxDir), newEntry(start, sigTraces, formatJSON),
		spansBody(t, map[string]any{"name": "append_items", "traceId": "long-turn"})); err != nil {
		t.Fatal(err)
	}
	rt.pass(t.Context())
	clock = start.Add(10 * time.Minute) // far past the 30 s session hold
	rt.pass(t.Context())
	if routed[machineRoute] != 0 {
		t.Fatal("a child span went to the machine project while its turn was still running")
	}
	h.recordSession(codexID, repoPath("codex"))
	if err := writeEntry(filepath.Join(dir, inboxDir), newEntry(clock, sigLogs, formatJSON),
		logsBody(t, logRecord("codex.sse_event", "long-turn", strAttr("conversation.id", codexID)))); err != nil {
		t.Fatal(err)
	}
	rt.pass(t.Context())
	rt.pass(t.Context())
	if routed["proj-codex"] < 2 {
		t.Fatalf("the turn's child span did not follow its trace to the project: %v", routed)
	}
}
