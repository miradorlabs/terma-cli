package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// OpenCode through the relay: terma's plugin exports OTLP/JSON with session.id and
// calls terma's hooks itself, so this is the JSON path end to end. Every scenario runs
// on each build of TERMA_E2E_OPENCODE_VERSIONS, the plugin written by `terma relay
// setup`, as setup writes it: running this terma by its absolute path.

const (
	openCodePrompt = "Reply exactly TERMA_TELEMETRY_REPLY. TERMA_OPENCODE_PROMPT"
	// openCodeScope and openCodeWire are the plugin's instrumentation scope and the wire
	// contract its version names.
	openCodeScope = "terma-opencode"
	openCodeWire  = "2"
)

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

// openCodeSpans are the plugin's spans of session sid upstream received, those whose name
// starts with prefix.
func openCodeSpans(e telemetryEvidence, sid, prefix string) []Span {
	var out []Span
	for _, s := range e.spans {
		if s.Scope == openCodeScope && s.Attrs["session.id"] == sid && strings.HasPrefix(s.Name, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// openCodeCalls are session sid's model calls of the conversation: its chat spans but the
// title's and a compaction's.
func openCodeCalls(e telemetryEvidence, sid string) []Span {
	return slices.DeleteFunc(openCodeSpans(e, sid, "chat "), func(s Span) bool { return s.Attrs["opencode.call.kind"] != "" })
}

// openCodeRecords are the plugin's log records of session sid named event.
func openCodeRecords(e telemetryEvidence, sid, event string) []LogRecord {
	var out []LogRecord
	for _, r := range e.logs {
		if r.Scope == openCodeScope && r.Attrs["session.id"] == sid && r.Attrs["event.name"] == event {
			out = append(out, r)
		}
	}
	return out
}

// termaDelivered are the hook events named name of session sid that a flush delivered
// upstream: never what still waits in the spool.
func termaDelivered(e telemetryEvidence, name, sid string) []LogRecord {
	var out []LogRecord
	for _, r := range e.logs {
		if fromTerma(r) && r.EventName == name && r.Attrs["session.id"] == sid {
			out = append(out, r)
		}
	}
	return out
}

// openCodeLeaks names every attribute and body of the plugin's spans and records that
// carries a marker.
func openCodeLeaks(e telemetryEvidence, markers ...string) []string {
	var out []string
	hit := func(where, v string) {
		for _, m := range markers {
			if strings.Contains(v, m) {
				out = append(out, where)
				return
			}
		}
	}
	for _, s := range e.spans {
		if s.Scope == openCodeScope {
			for k, v := range s.Attrs {
				hit(s.Name+" "+k, v)
			}
		}
	}
	for _, r := range e.logs {
		if r.Scope == openCodeScope {
			for k, v := range r.Attrs {
				hit(r.Attrs["event.name"]+" "+k, v)
			}
			hit(r.Attrs["event.name"]+" body", r.Body)
		}
	}
	return out
}

// waitUntil polls ok for up to timeout and reports whether it came true.
func waitUntil(timeout time.Duration, ok func() bool) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(250 * time.Millisecond) {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// openCodeRelay is a sandbox whose OpenCode talks to a scripted provider answering with
// steps, and exports through a running relay under a team policy that collects content or
// withholds it. scripted builds the steps once the sandbox's folders are known.
func openCodeRelay(t *testing.T, content bool, scripted func(sb *Sandbox) []openCodeStep, opts ...func(sb *Sandbox)) *Sandbox {
	t.Helper()
	track(t)
	sb := New(t, Isolated)
	var calls atomic.Int32
	var steps []openCodeStep
	if scripted != nil {
		steps = scripted(sb)
	}
	provider := httptest.NewServer(openCodeScriptedProvider(&calls, steps...))
	t.Cleanup(provider.Close)
	sb.UseOpenCodeProvider(provider.URL)
	for _, o := range opts {
		o(sb)
	}
	sb.UseRelay(RelayOptions{Start: true, Content: content})
	return sb
}

// finishOpenCode stops the relay and fails a scenario whose withheld content met a key no
// rule names.
func finishOpenCode(t *testing.T, sb *Sandbox) {
	t.Helper()
	sb.StopRelay()
	c := sb.RelayStats()
	noteRelayStats(t.Name(), c)
	failUnclassified(t, c)
}

func TestRelayOpenCode(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "relay.telemetry")
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				Proves(t, b.Harness, b.Version, map[bool]string{true: "relay.content_allowed", false: "relay.content_withheld"}[content])
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
				// The reply's length leaves whatever the policy, so a withheld reply still counts.
				if !slices.ContainsFunc(openCodeCalls(e, sid), func(s Span) bool { return s.Attrs["opencode.completion.length"] != "" }) {
					t.Errorf("no model call carried the reply's length")
				}
				finishOpenCode(t, sb)
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
		ProvesAll(t, b, "relay.only_opted_in")
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
		finishOpenCode(t, sb)
	})
}

// One run with a shell call and a file write: each model call is a chat span of the plugin's
// version 2 wire with its own id, each tool call a span with its status and an id qualified
// by its assistant message, and the write a terma.files.touched naming that same id. The
// idle hook at the turn's end starts the flush: the hook records reach upstream with no
// `terma spool flush` of the test's. With content withheld, no prompt, reply, argument,
// result or file path leaves in the plugin's export, and the reply's length does.
func TestOpenCodeToolsAndEdit(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				const file = "TERMA_WRITTEN.txt"
				sb := openCodeRelay(t, content, func(sb *Sandbox) []openCodeStep {
					return []openCodeStep{
						{calls: []openCodeCall{{id: "call_sh", name: "bash", args: map[string]any{"command": "printf TERMA_TELEMETRY_TOOL", "description": "scripted"}}}},
						{calls: []openCodeCall{{id: "call_w", name: "write", args: map[string]any{"filePath": filepath.Join(sb.Repo, file), "content": "TERMA_TELEMETRY_TOOL\n"}}}},
					}
				})
				sid := sb.OpenCodeRun(b, sb.Repo, "", "Do the task. TERMA_OPENCODE_PROMPT")
				var e telemetryEvidence
				arrived := waitUntil(45*time.Second, func() bool {
					e = sb.Receiver.evidence()
					return len(termaDelivered(e, "terma.files.touched", sid)) > 0 && len(openCodeCalls(e, sid)) >= 3 &&
						len(openCodeSpans(e, sid, "execute_tool ")) >= 2 && len(openCodeRecords(e, sid, "opencode.user_prompt")) > 0
				})
				if !arrived {
					// Doctor says why the relay holds what it received (an unread policy or key, a
					// sender its claim does not cover).
					t.Fatalf("the session did not arrive whole: %d calls, %d tool spans, %d prompts, %d touches; relay %v\ndoctor:\n%s",
						len(openCodeCalls(e, sid)), len(openCodeSpans(e, sid, "execute_tool ")), len(openCodeRecords(e, sid, "opencode.user_prompt")),
						len(termaDelivered(e, "terma.files.touched", sid)), sb.RelayStats(), sb.doctor())
				}
				ids := map[string]bool{}
				for _, s := range openCodeSpans(e, sid, "chat ") {
					if s.ScopeVersion != openCodeWire || s.Attrs["opencode.call.id"] == "" {
						t.Errorf("%s: scope version %q, call id %q", s.Name, s.ScopeVersion, s.Attrs["opencode.call.id"])
					}
					ids[s.Attrs["opencode.call.id"]] = true
				}
				if len(ids) < 4 {
					t.Errorf("model calls by id: %v, want three of the conversation and the title's", ids)
				}
				byCall := map[string]Span{}
				for _, s := range openCodeSpans(e, sid, "execute_tool ") {
					id := s.Attrs["gen_ai.tool.call.id"]
					message, provider, _ := strings.Cut(id, ":")
					if !strings.HasPrefix(message, "msg_") || s.Attrs["opencode.tool.status"] != "completed" {
						t.Errorf("%s: call id %q, status %q", s.Name, id, s.Attrs["opencode.tool.status"])
					}
					byCall[provider] = s
				}
				write, ok := byCall["call_w"]
				if _, shell := byCall["call_sh"]; !shell || !ok {
					t.Fatalf("tool spans by the provider's call id: %v", slices.Collect(maps.Keys(byCall)))
				}
				var touched []string
				for _, r := range termaDelivered(e, "terma.files.touched", sid) {
					touched = append(touched, r.Attrs["gen_ai.tool.call.id"])
				}
				if want := []string{write.Attrs["gen_ai.tool.call.id"]}; !slices.Equal(touched, want) {
					t.Errorf("files touched by calls %q, want once by the write's %q", touched, want)
				}
				if !slices.ContainsFunc(openCodeCalls(e, sid), func(s Span) bool { return s.Attrs["opencode.completion.length"] != "" }) {
					t.Errorf("no model call carried the reply's length")
				}
				leaks := openCodeLeaks(e, "TERMA_OPENCODE_PROMPT", "TERMA_TELEMETRY_REPLY", "TERMA_TELEMETRY_TOOL", file)
				if !content && len(leaks) > 0 {
					t.Errorf("content withheld, but it left in: %v", leaks)
				}
				if content && len(leaks) == 0 {
					t.Errorf("content collected, but none of it arrived")
				}
				finishOpenCode(t, sb)
			})
		}
	})
}

// A second process continues the session: its hooks claim it again, so its call is
// forwarded, not held as a process no claim covers, and the relay drops nothing.
func TestOpenCodeResume(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		sb := openCodeRelay(t, true, nil)
		sid := sb.OpenCodeRun(b, sb.Repo, "", "TERMA_OPENCODE_PROMPT first")
		if again := sb.OpenCodeRun(b, sb.Repo, sid, "TERMA_OPENCODE_PROMPT second"); again != sid {
			t.Fatalf("the second run continued %s, want %s", again, sid)
		}
		var e telemetryEvidence
		if !waitUntil(45*time.Second, func() bool {
			e = sb.Receiver.evidence()
			return len(openCodeRecords(e, sid, "opencode.user_prompt")) >= 2 && len(openCodeCalls(e, sid)) >= 2
		}) {
			t.Fatalf("the continued session arrived with %d prompts and %d calls; relay %v",
				len(openCodeRecords(e, sid, "opencode.user_prompt")), len(openCodeCalls(e, sid)), sb.RelayStats())
		}
		c := sb.RelayStats()
		for k, v := range c {
			if strings.HasPrefix(k, "dropped.") && v > 0 {
				t.Errorf("the relay dropped %d: %s", v, k)
			}
		}
		finishOpenCode(t, sb)
	})
}

// A provider that numbers its call ids per response gives call_0 twice: the plugin
// qualifies each by its assistant message, so two calls arrive as two.
func TestOpenCodeRepeatedToolCallID(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		sb := openCodeRelay(t, true, func(*Sandbox) []openCodeStep {
			return []openCodeStep{
				{calls: []openCodeCall{{id: "call_0", name: "bash", args: map[string]any{"command": "printf one", "description": "first"}}}},
				{calls: []openCodeCall{{id: "call_0", name: "bash", args: map[string]any{"command": "printf two", "description": "second"}}}},
			}
		})
		sid := sb.OpenCodeRun(b, sb.Repo, "", "Run both. TERMA_OPENCODE_PROMPT")
		var spans []Span
		waitUntil(45*time.Second, func() bool {
			spans = openCodeSpans(sb.Receiver.evidence(), sid, "execute_tool bash")
			return len(spans) >= 2
		})
		ids := map[string]bool{}
		for _, s := range spans {
			id := s.Attrs["gen_ai.tool.call.id"]
			if !strings.HasSuffix(id, ":call_0") {
				t.Errorf("tool call id %q does not end in the provider's", id)
			}
			ids[id] = true
		}
		if len(spans) != 2 || len(ids) != 2 {
			t.Errorf("%d bash spans with %d ids, want two of each: %v", len(spans), len(ids), ids)
		}
		finishOpenCode(t, sb)
	})
}

// An MCP tool's span names its server and tool, which OpenCode 1 joins into one name, and
// carries the server's real result.
func TestOpenCodeMCP(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		const server = "termaecho"
		sb := openCodeRelay(t, true, func(*Sandbox) []openCodeStep {
			return []openCodeStep{{calls: []openCodeCall{{id: "call_mcp", name: server + "_echo", args: map[string]any{"text": "TERMA_MCP_CANARY"}}}}}
		}, func(sb *Sandbox) { sb.UseOpenCodeMCP(server) })
		sid := sb.OpenCodeRun(b, sb.Repo, "", "Echo it. TERMA_OPENCODE_PROMPT")
		var spans []Span
		waitUntil(45*time.Second, func() bool {
			spans = openCodeSpans(sb.Receiver.evidence(), sid, "execute_tool ")
			return len(spans) > 0
		})
		if len(spans) != 1 {
			t.Fatalf("%d tool spans, want the MCP call's; relay %v", len(spans), sb.RelayStats())
		}
		a := spans[0].Attrs
		if a["opencode.mcp.server"] != server || a["opencode.mcp.tool"] != "echo" || a["opencode.tool.status"] != "completed" {
			t.Errorf("the MCP call: server %q, tool %q, status %q", a["opencode.mcp.server"], a["opencode.mcp.tool"], a["opencode.tool.status"])
		}
		if !strings.Contains(a["gen_ai.tool.call.result"], "echo: TERMA_MCP_CANARY") {
			t.Errorf("the MCP call's result is %q", a["gen_ai.tool.call.result"])
		}
		finishOpenCode(t, sb)
	})
}

// A subagent's session names the session that opened it, and the task tool's span names
// the subagent's, with content withheld too.
func TestOpenCodeSubagent(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		sb := openCodeRelay(t, false, func(*Sandbox) []openCodeStep {
			return []openCodeStep{
				{calls: []openCodeCall{{id: "call_task", name: "task", args: map[string]any{"description": "scripted child",
					"prompt": "Reply exactly TERMA_TELEMETRY_REPLY. TERMA_OPENCODE_PROMPT", "subagent_type": "general"}}}},
			}
		})
		sid := sb.OpenCodeRun(b, sb.Repo, "", "Delegate it. TERMA_OPENCODE_PROMPT")
		var e telemetryEvidence
		var child string
		waitUntil(45*time.Second, func() bool {
			e = sb.Receiver.evidence()
			for _, s := range openCodeSpans(e, sid, "execute_tool task") {
				child = s.Attrs["opencode.tool.subagent_session.id"]
			}
			return child != "" && len(openCodeCalls(e, child)) > 0
		})
		if child == "" {
			t.Fatalf("no task span named its subagent; relay %v", sb.RelayStats())
		}
		calls := openCodeCalls(e, child)
		if len(calls) == 0 {
			t.Fatalf("nothing of subagent %s arrived", child)
		}
		for _, s := range calls {
			if s.Attrs["opencode.parent_session.id"] != sid {
				t.Errorf("the subagent's %s names parent %q, want %s", s.Name, s.Attrs["opencode.parent_session.id"], sid)
			}
		}
		if !slices.ContainsFunc(calls, func(s Span) bool { return s.Attrs["opencode.completion.length"] != "" }) {
			t.Errorf("the subagent's reply arrived with no length")
		}
		if leaks := openCodeLeaks(e, "TERMA_OPENCODE_PROMPT", "TERMA_TELEMETRY_REPLY"); len(leaks) > 0 {
			t.Errorf("content withheld, but it left in: %v", leaks)
		}
		finishOpenCode(t, sb)
	})
}

// The plugin runs terma by the absolute path setup recorded, so a run whose PATH holds no
// terma is still claimed and recorded.
func TestOpenCodeTermaOffPath(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		sb := openCodeRelay(t, true, nil)
		var path []string
		for _, kv := range sb.termaEnv() {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				for dir := range strings.SplitSeq(v, string(os.PathListSeparator)) {
					if _, err := os.Stat(filepath.Join(dir, "terma")); err != nil && dir != filepath.Dir(sb.Terma) {
						path = append(path, dir)
					}
				}
			}
		}
		sid := sb.OpenCodeRunWith(b, sb.Repo, "", openCodePrompt, []string{"PATH=" + strings.Join(path, string(os.PathListSeparator))})
		var e telemetryEvidence
		if !waitUntil(30*time.Second, func() bool {
			e = sb.Receiver.evidence()
			return len(openCodeCalls(e, sid)) > 0 && len(openCodeRecords(e, sid, "opencode.user_prompt")) > 0
		}) {
			t.Fatalf("a run with no terma on its PATH was not recorded; relay %v", sb.RelayStats())
		}
		var failures contractFailures
		checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
		for _, f := range failures {
			t.Error(f)
		}
		finishOpenCode(t, sb)
	})
}

// The terma setup recorded is moved away: the plugin can claim no session, so nothing of
// it reaches upstream; it says so once in OpenCode's log, and `terma doctor` names the
// path that is gone.
func TestOpenCodeTermaRemoved(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		sb := openCodeRelay(t, true, nil)
		plugin := filepath.Join(sb.Home, ".config", "opencode", "plugins", "terma.js")
		src, err := os.ReadFile(plugin)
		if err != nil {
			t.Fatal(err)
		}
		// The terma the plugin runs, as setup recorded it, is pointed at a copy that is then
		// removed: the plugin stays byte for byte what this build writes for its configuration.
		moved := filepath.Join(sb.Dir, "moved-terma", "terma")
		recorded, _ := json.Marshal(abs(sb.Terma))
		gone, _ := json.Marshal(moved)
		if !strings.Contains(string(src), `"hookCommand":[`+string(recorded)) {
			t.Fatalf("the plugin does not run %s", sb.Terma)
		}
		sb.writeAbs(plugin, strings.Replace(string(src), `"hookCommand":[`+string(recorded), `"hookCommand":[`+string(gone), 1))

		sid := sb.OpenCodeRun(b, sb.Repo, "", openCodePrompt)
		time.Sleep(8 * time.Second)
		e := sb.Receiver.evidence()
		if n := openCodeAgentRecords(e, sid); n != 0 {
			t.Errorf("a session no hook could claim reached upstream: %d records", n)
		}
		for _, r := range e.logs {
			if fromTerma(r) && r.Attrs["session.id"] == sid {
				t.Errorf("a hook record of the session reached upstream: %s", r.EventName)
			}
		}

		var warnings, seen []string
		logs, _ := filepath.Glob(filepath.Join(sb.Home, ".local", "share", "opencode", "log", "*"))
		for _, path := range logs {
			data, _ := os.ReadFile(path)
			seen = append(seen, fmt.Sprintf("%s (%d bytes, %d lines naming terma)", filepath.Base(path), len(data), strings.Count(string(data), "terma")))
			for line := range strings.Lines(string(data)) {
				if strings.Contains(line, "terma: cannot run") {
					warnings = append(warnings, strings.TrimSpace(line))
				}
			}
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], moved) {
			t.Errorf("OpenCode's log holds %d warnings of terma's, want one naming %s: %q (log files: %q)", len(warnings), moved, warnings, seen)
		}

		// Doctor looks at OpenCode only where it is on the PATH: the sandbox's own bin folder,
		// which binDir names as a PATH entry.
		if err := os.Symlink(b.Path, filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "opencode")); err != nil {
			t.Fatal(err)
		}
		if out := sb.doctor(); !strings.Contains(out, moved) {
			t.Errorf("terma doctor does not name %s:\n%s", moved, out)
		}
		finishOpenCode(t, sb)
	})
}

// How long the plugin's reach check takes with the real terma: the plugin holds a tool for
// it and gives it REACH_HOOK_TIMEOUT_MS. This logs the times and asserts none: it needs
// terma only, so it runs once, not on each OpenCode build. It fails only if a hook does.
func TestOpenCodeReachHookTime(t *testing.T) {
	sb := openCodeRelay(t, true, nil)
	const sid = "ses_reach_timing"
	hook := func(event string, payload map[string]any) time.Duration {
		t.Helper()
		body, _ := json.Marshal(payload)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, sb.Terma, "hook", event)
		cmd.Dir = sb.Repo
		cmd.Env = sb.termaEnv()
		cmd.Stdin = bytes.NewReader(body)
		began := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(began)
		if err != nil {
			t.Fatalf("terma hook %s: %v\n%s", event, err, out)
		}
		return took
	}
	t.Logf("opencode-session-start: %v", hook("opencode-session-start", map[string]any{"session_id": sid, "cwd": sb.Repo}))
	for _, c := range []struct {
		name  string
		reach map[string]any
	}{
		{"read", map[string]any{"tool": "read", "paths": []string{filepath.Join(sb.Repo, "README.md")}}},
		{"bash", map[string]any{"tool": "bash", "command": "cat README.md"}},
	} {
		var times []time.Duration
		for range 20 {
			payload := map[string]any{"session_id": sid, "cwd": sb.Repo}
			maps.Copy(payload, c.reach)
			times = append(times, hook("opencode-tool-reach", payload))
		}
		first := times[0]
		slices.Sort(times)
		t.Logf("opencode-tool-reach %s, %d runs: first %v; min %v, median %v, max %v", c.name, len(times), first, times[0], times[len(times)/2], times[len(times)-1])
	}
	finishOpenCode(t, sb)
}

// UseOpenCodeMCP adds the echo MCP server to the sandbox's OpenCode config as server, so
// OpenCode offers its tool as <server>_echo.
func (sb *Sandbox) UseOpenCodeMCP(server string) {
	t := sb.T
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	editJSON(t, filepath.Join(sb.Home, ".config", "opencode", "opencode.json"), func(doc map[string]any) {
		doc["mcp"] = map[string]any{server: map[string]any{"type": "local", "command": []string{exe, echoMCPArg}, "enabled": true}}
	})
}
