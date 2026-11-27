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
	"runtime"
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

// describeToolSpans says what each tool call reported, for a failure message: its status,
// denial, exit code, files and the start of its result.
func describeToolSpans(spans []Span) string {
	var out []string
	for _, s := range spans {
		result := s.Attrs["gen_ai.tool.call.result"]
		if len(result) > 160 {
			result = result[:160]
		}
		out = append(out, fmt.Sprintf("%s status=%q denial=%q exit=%q files=%q result=%q args=%q", s.Name,
			s.Attrs["opencode.tool.status"], s.Attrs["opencode.tool.denial"], s.Attrs["opencode.tool.exit_code"],
			s.Attrs["opencode.tool.files"], result, s.Attrs["gen_ai.tool.call.arguments"]))
	}
	return strings.Join(out, "; ")
}

// describeTermaRecords lists every record terma itself delivered, by name and session, for a
// failure message.
func describeTermaRecords(e telemetryEvidence) string {
	var out []string
	for _, r := range e.logs {
		if fromTerma(r) {
			out = append(out, r.EventName+"@"+r.Attrs["session.id"])
		}
	}
	return strings.Join(out, ", ")
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

// openCodeRelay is a sandbox whose OpenCode, b, talks to a scripted provider answering with
// steps, chosen at `terma setup`, and exports through a running relay under a team policy
// that collects content or withholds it. scripted builds the steps once the sandbox's folders
// are known.
func openCodeRelay(t *testing.T, b Binary, content bool, scripted func(sb *Sandbox) []openCodeStep, opts ...func(sb *Sandbox)) *Sandbox {
	t.Helper()
	track(t)
	return openCodeRelayIn(New(t, Isolated), b, content, scripted, opts...)
}

// openCodeRelayIn is openCodeRelay in a sandbox the scenario built.
func openCodeRelayIn(sb *Sandbox, b Binary, content bool, scripted func(sb *Sandbox) []openCodeStep, opts ...func(sb *Sandbox)) *Sandbox {
	t := sb.T
	t.Helper()
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
	sb.UseOpenCode(b)
	sb.UseRelay(RelayOptions{Start: true, Content: content})
	return sb
}

// paused reports whether terma holds session's run in force as Mixed: its run ledger in
// the state folder, which doctor counts but no longer lists by session.
func (sb *Sandbox) paused(session string) bool {
	sb.T.Helper()
	data, err := os.ReadFile(filepath.Join(sb.TermaConfig, "runs", session+".json"))
	if err != nil {
		return false
	}
	var ledger struct {
		Runs []struct {
			MixedAt time.Time `json:"mixed_at"`
		} `json:"runs"`
	}
	if json.Unmarshal(data, &ledger) != nil || len(ledger.Runs) == 0 {
		return false
	}
	return !ledger.Runs[len(ledger.Runs)-1].MixedAt.IsZero()
}

// finishOpenCode stops the relay and fails a scenario whose withheld content met a key no
// rule names, or whose export puts a call before the prompt that asked for it.
func finishOpenCode(t *testing.T, sb *Sandbox) {
	t.Helper()
	sb.StopRelay()
	c := sb.RelayStats()
	noteRelayStats(t.Name(), c)
	failUnclassified(t, c)
	checkOpenCodePromptOrder(t, sb.Receiver.evidence())
	// A hook runs in a process group of its own, so OpenCode's signal at a step's end no
	// longer stops it: each must still end by itself or at the plugin's deadline.
	if !waitUntil(10*time.Second, func() bool { return len(sb.termaHookProcesses()) == 0 }) {
		t.Errorf("terma hook processes of the sandbox were left running: %v", sb.termaHookProcesses())
	}
}

// checkOpenCodePromptOrder holds every prompt upstream received to its place in time: a
// reader that orders a session by time finds each model call, tool call and title call under
// the prompt that started it, so none of them starts before that prompt's own time.
func checkOpenCodePromptOrder(t *testing.T, e telemetryEvidence) {
	t.Helper()
	for _, r := range e.logs {
		if r.Scope != openCodeScope || r.Attrs["event.name"] != "opencode.user_prompt" {
			continue
		}
		sid, id := r.Attrs["session.id"], r.Attrs["opencode.message.id"]
		at := time.Unix(0, int64(r.Proto.GetTimeUnixNano()))
		n, first := 0, time.Time{}
		for _, s := range e.spans {
			if s.Scope != openCodeScope || s.Attrs["session.id"] != sid || id == "" {
				continue
			}
			if s.Attrs["opencode.parent_message.id"] != id && s.Attrs["opencode.call.id"] != "title:"+id {
				continue
			}
			start := time.Unix(0, int64(s.Proto.GetStartTimeUnixNano()))
			if n++; first.IsZero() || start.Before(first) {
				first = start
			}
			if start.Before(at) {
				t.Errorf("session %s: %q starts %s before the prompt %s that asked for it", sid, s.Name, at.Sub(start), id)
			}
			if end := time.Unix(0, int64(s.Proto.GetEndTimeUnixNano())); end.Before(start) {
				t.Errorf("session %s: %q ends %s before it starts", sid, s.Name, start.Sub(end))
			}
		}
		if n > 0 {
			t.Logf("session %s prompt %s: stamped %s, its first of %d spans starts %s later", sid, id, at.UTC().Format("15:04:05.000"), n, first.Sub(at).Round(time.Millisecond))
		}
	}
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
				sb.UseOpenCode(b)
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
		sb.UseOpenCode(b)
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
				sb := openCodeRelay(t, b, content, func(sb *Sandbox) []openCodeStep {
					return []openCodeStep{
						{calls: []openCodeCall{openCodeCallFor(b, "call_sh", openCodeIntent{shell: "printf TERMA_TELEMETRY_TOOL", description: "scripted"})}},
						{calls: []openCodeCall{openCodeCallFor(b, "call_w", openCodeIntent{writePath: filepath.Join(sb.Repo, file), content: "TERMA_TELEMETRY_TOOL\n"})}},
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
					t.Fatalf("the session did not arrive whole: %d calls, %d tool spans, %d prompts, %d touches; relay %v\ntool spans: %s\nterma's records: %s\ndoctor:\n%s",
						len(openCodeCalls(e, sid)), len(openCodeSpans(e, sid, "execute_tool ")), len(openCodeRecords(e, sid, "opencode.user_prompt")),
						len(termaDelivered(e, "terma.files.touched", sid)), sb.RelayStats(), describeToolSpans(openCodeSpans(e, sid, "execute_tool ")), describeTermaRecords(e), sb.doctor())
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
				// The call that writes the title: OpenCode 2 reports its usage, which the plugin gives
				// it; OpenCode 1 gives a plugin nothing of it, so it has no tokens there.
				var titles []Span
				for _, s := range openCodeSpans(e, sid, "chat ") {
					if s.Attrs["opencode.call.kind"] == "title" {
						titles = append(titles, s)
					}
				}
				if len(titles) != 1 {
					t.Errorf("%d title calls, want one", len(titles))
				}
				for _, s := range titles {
					if tokens := s.Attrs["gen_ai.usage.input_tokens"]; (tokens != "") != openCode2(b) {
						t.Errorf("OpenCode %s's title call has input tokens %q", b.Version, tokens)
					}
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
		sb := openCodeRelay(t, b, true, nil)
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
		first := openCodeCallFor(b, "call_0", openCodeIntent{shell: "printf one", description: "first"})
		second := openCodeCallFor(b, "call_0", openCodeIntent{shell: "printf two", description: "second"})
		sb := openCodeRelay(t, b, true, func(*Sandbox) []openCodeStep {
			return []openCodeStep{{calls: []openCodeCall{first}}, {calls: []openCodeCall{second}}}
		})
		sid := sb.OpenCodeRun(b, sb.Repo, "", "Run both. TERMA_OPENCODE_PROMPT")
		var spans []Span
		waitUntil(45*time.Second, func() bool {
			spans = openCodeSpans(sb.Receiver.evidence(), sid, "execute_tool "+first.name)
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
			t.Errorf("%d %s spans with %d ids, want two of each: %v", len(spans), first.name, len(ids), ids)
		}
		finishOpenCode(t, sb)
	})
}

// An MCP tool's span names its server and tool, which OpenCode 1 joins into one name, and
// carries the server's real result. OpenCode 2 offers an MCP tool only in code mode: the model
// calls `execute` with code that calls it. 2.4.1 reports that inner call to a plugin, so it is
// a span of its own beside execute's, with both names and its real result; 2.0.0 reports
// nothing of it, so execute's is the only span there.
func TestOpenCodeMCP(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		const server = "termaecho"
		call := openCodeCallFor(b, "call_mcp", openCodeIntent{mcpServer: server, mcpTool: "echo", mcpArgs: map[string]any{"text": "TERMA_MCP_CANARY"}})
		sb := openCodeRelay(t, b, true, func(*Sandbox) []openCodeStep {
			return []openCodeStep{{calls: []openCodeCall{call}}}
		}, func(sb *Sandbox) { sb.UseOpenCodeMCP(server) })
		sid := sb.OpenCodeRun(b, sb.Repo, "", "Echo it. TERMA_OPENCODE_PROMPT")
		want := 1
		if openCode2(b) && !versionLess(b.Version, "2.4.1") {
			want = 2
		}
		var spans []Span
		waitUntil(45*time.Second, func() bool {
			spans = openCodeSpans(sb.Receiver.evidence(), sid, "execute_tool ")
			return len(spans) >= want
		})
		byName := map[string]Span{}
		for _, s := range spans {
			byName[s.Attrs["gen_ai.tool.name"]] = s
		}
		if len(spans) != want || len(byName) != want {
			t.Fatalf("tool spans %v, want %d; relay %v", slices.Collect(maps.Keys(byName)), want, sb.RelayStats())
		}
		if openCode2(b) {
			execute, ok := byName["execute"]
			if !ok || execute.Attrs["opencode.mcp.server"] != "" || execute.Attrs["opencode.tool.status"] != "completed" {
				t.Errorf("the execute call: %v", execute.Attrs)
			}
			// Whether the build ran the MCP tool at all: 2.0.0 does not offer it to code mode.
			t.Logf("execute's result on %s: %s", b.Version, execute.Attrs["gen_ai.tool.call.result"])
			if want == 1 {
				finishOpenCode(t, sb)
				return
			}
		}
		mcp, ok := byName[server+"_echo"]
		if !ok {
			t.Fatalf("no span of the MCP call: %v", slices.Collect(maps.Keys(byName)))
		}
		a := mcp.Attrs
		if a["opencode.mcp.server"] != server || a["opencode.mcp.tool"] != "echo" || a["opencode.tool.status"] != "completed" {
			t.Errorf("the MCP call: server %q, tool %q, status %q", a["opencode.mcp.server"], a["opencode.mcp.tool"], a["opencode.tool.status"])
		}
		if !strings.Contains(a["gen_ai.tool.call.result"], "echo: TERMA_MCP_CANARY") {
			t.Errorf("the MCP call's result is %q", a["gen_ai.tool.call.result"])
		}
		finishOpenCode(t, sb)
	})
}

// A subagent's session names the session that opened it on each of its spans and records,
// and in its terma.session.start, and the call that launched it (task on OpenCode 1,
// subagent on OpenCode 2) names the subagent's session, with content withheld too. On 2.4.1
// the subagent runs in the background and the launching call returns at once; on 2.0.0 and
// OpenCode 1 it runs before the call returns.
func TestOpenCodeSubagent(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		launch := openCodeCallFor(b, "call_task", openCodeIntent{subagent: "Reply exactly TERMA_TELEMETRY_REPLY. TERMA_OPENCODE_PROMPT", description: "scripted child"})
		sb := openCodeRelay(t, b, false, func(*Sandbox) []openCodeStep {
			return []openCodeStep{{calls: []openCodeCall{launch}}}
		})
		sid := sb.OpenCodeRun(b, sb.Repo, "", "Delegate it. TERMA_OPENCODE_PROMPT")
		var e telemetryEvidence
		var child string
		waitUntil(45*time.Second, func() bool {
			e = sb.Receiver.evidence()
			for _, s := range openCodeSpans(e, sid, "execute_tool "+launch.name) {
				child = s.Attrs["opencode.tool.subagent_session.id"]
			}
			return child != "" && len(openCodeCalls(e, child)) > 0 && len(termaDelivered(e, "terma.session.start", child)) > 0
		})
		if child == "" {
			t.Fatalf("no %s span named its subagent; relay %v", launch.name, sb.RelayStats())
		}
		calls := openCodeCalls(e, child)
		if len(calls) == 0 {
			t.Fatalf("nothing of subagent %s arrived", child)
		}
		for _, s := range openCodeSpans(e, child, "") {
			if s.Attrs["opencode.parent_session.id"] != sid {
				t.Errorf("the subagent's %s names parent %q, want %s", s.Name, s.Attrs["opencode.parent_session.id"], sid)
			}
		}
		for _, r := range e.logs {
			if r.Scope == openCodeScope && r.Attrs["session.id"] == child && r.Attrs["opencode.parent_session.id"] != sid {
				t.Errorf("the subagent's %s names parent %q, want %s", r.Attrs["event.name"], r.Attrs["opencode.parent_session.id"], sid)
			}
		}
		starts := termaDelivered(e, "terma.session.start", child)
		if len(starts) == 0 {
			t.Errorf("no terma.session.start of the subagent arrived")
		}
		for _, r := range starts {
			if r.Attrs["terma.session.parent.id"] != sid {
				t.Errorf("the subagent's terma.session.start names parent %q, want %s: %v", r.Attrs["terma.session.parent.id"], sid, r.Attrs)
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
		sb := openCodeRelay(t, b, true, nil)
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
// it reaches upstream, and `terma doctor` names the path that is gone. On OpenCode 1 the
// plugin says so once in OpenCode's log; OpenCode 2 gives a plugin no log, and its warning
// goes to the server's stderr, which no file keeps.
func TestOpenCodeTermaRemoved(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		sb := openCodeRelay(t, b, true, nil)
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

		if !openCode2(b) {
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
		}

		// Doctor looks at OpenCode only where it is on the PATH, where UseOpenCode put it.
		if out := sb.doctor(); !strings.Contains(out, moved) {
			t.Errorf("terma doctor does not name %s:\n%s", moved, out)
		}
		finishOpenCode(t, sb)
	})
}

// `opencode run -f <file>` on OpenCode 2 puts the file's content into the prompt and gives a
// plugin no path to check what the session read, so the plugin fails closed: nothing of that
// session leaves it, and the file's content is nowhere upstream. A run without -f in the same
// sandbox is recorded as usual.
func TestOpenCodeAttachedFileOnCommandLine(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		if !openCode2(b) {
			t.Skip("OpenCode 1 gives a plugin an attached file's path")
		}
		sb := openCodeRelay(t, b, true, nil)
		attached := filepath.Join(sb.Repo, "attached.txt")
		sb.writeAbs(attached, "TERMA_ATTACHED_MARKER\n")
		unread := sb.openCodeRun(b, sb.Repo, "", "Read the attached file. TERMA_OPENCODE_PROMPT", nil, false, []string{"-f", attached})
		// The cutoff is terma's, not only the plugin's memory: the run is Mixed for a read
		// terma cannot place, which doctor lists as the one paused session.
		if !sb.paused(unread) {
			t.Errorf("terma does not hold the run with -f as Mixed")
		}
		if out := sb.doctor(); !strings.Contains(out, "1 session is paused") || !strings.Contains(out, "ran a command terma could not trace") {
			t.Errorf("terma doctor does not list the run with -f as paused:\n%s", out)
		}
		recorded := sb.OpenCodeRun(b, sb.Repo, "", openCodePrompt)
		var e telemetryEvidence
		if !waitUntil(45*time.Second, func() bool {
			e = sb.Receiver.evidence()
			return len(openCodeCalls(e, recorded)) > 0 && len(openCodeRecords(e, recorded, "opencode.user_prompt")) > 0
		}) {
			t.Fatalf("the run without -f was not recorded; relay %v", sb.RelayStats())
		}
		// What arrives a little later would be the attached run's: give it the time.
		time.Sleep(5 * time.Second)
		e = sb.Receiver.evidence()
		for _, s := range e.spans {
			if s.Scope == openCodeScope && s.Attrs["session.id"] == unread {
				t.Errorf("the run with -f exported span %s", s.Name)
			}
		}
		for _, r := range e.logs {
			if r.Scope == openCodeScope && r.Attrs["session.id"] == unread {
				t.Errorf("the run with -f exported record %s", r.Attrs["event.name"])
			}
		}
		if leaked := leakedFieldsOf(e, "TERMA_ATTACHED_MARKER"); len(leaked) > 0 {
			t.Errorf("the attached file's content reached upstream: %v", leaked)
		}
		finishOpenCode(t, sb)
	})
}

// An edit into another organization's checkout, nested in the session's own folder so
// OpenCode asks no permission: a write, an edit and a subagent's write in one run, and a
// patch in a second run on a model OpenCode offers its patch tool to. terma checks each
// before it runs, the run goes Mixed at that check, and nothing of the call (its span, its
// arguments, the file's content) reaches upstream. (A checkout no ticked team collects
// belongs to no organization, so reaching it is no crossing: terma's routing, not this.)
func TestOpenCodeEditIntoAnotherCheckout(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		const (
			editPrompt  = "Make the changes. TERMA_OPENCODE_PROMPT"
			patchPrompt = "Patch it. TERMA_OPENCODE_PROMPT"
			childJob    = "TERMA_CHILD_JOB: write the file."
		)
		markers := []string{"TERMA_CROSS_WRITE", "TERMA_CROSS_EDIT", "TERMA_CROSS_SUB", "TERMA_CROSS_PATCH"}
		var other string
		track(t)
		const otherOrigin = "github.com/someone/other"
		sb := openCodeRelayIn(New(t, Isolated, WithOtherOrganization(otherOrigin)), b, true, nil, func(sb *Sandbox) {
			other = filepath.Join(sb.Repo, "other")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			sb.gitIn(other, "init", "-q", "-b", "main")
			sb.gitIn(other, "remote", "add", "origin", "https://"+otherOrigin+".git")
			sb.writeAbs(filepath.Join(other, "notes.txt"), "alpha\nbeta\n")
			call := func(id string, intent openCodeIntent) openCodeStep {
				return openCodeStep{calls: []openCodeCall{openCodeCallFor(b, id, intent)}}
			}
			provider := httptest.NewServer(openCodeConversationProvider([]openCodeConversation{
				{key: childJob, steps: []openCodeStep{
					call("call_sub", openCodeIntent{writePath: filepath.Join(other, "sub.txt"), content: "TERMA_CROSS_SUB\n"}),
				}},
				{key: patchPrompt, steps: []openCodeStep{
					call("call_patch", openCodeIntent{patchText: "*** Begin Patch\n*** Add File: " + filepath.Join(other, "patched.txt") + "\n+TERMA_CROSS_PATCH\n*** End Patch"}),
				}},
				{key: editPrompt, steps: []openCodeStep{
					call("call_write", openCodeIntent{writePath: filepath.Join(other, "w.txt"), content: "TERMA_CROSS_WRITE\n"}),
					call("call_edit", openCodeIntent{editPath: filepath.Join(other, "notes.txt"), oldString: "beta", newString: "TERMA_CROSS_EDIT"}),
					call("call_task", openCodeIntent{subagent: childJob, description: "scripted child"}),
				}},
			}))
			t.Cleanup(provider.Close)
			sb.UseOpenCodeProvider(provider.URL)
		})
		// The person ticks the other organization's team, so its checkout is that organization's.
		teams := exec.Command(sb.Terma, "teams", "--select", otherOrgProject)
		teams.Dir, teams.Env = sb.Repo, sb.termaEnv()
		if out, err := teams.CombinedOutput(); err != nil {
			t.Fatalf("terma teams: %v\n%s", err, out)
		}
		edited := sb.OpenCodeRun(b, sb.Repo, "", editPrompt)
		patched := sb.openCodeRun(b, sb.Repo, "", patchPrompt, nil, false, []string{"-m", "fake/gpt-5-m"})
		// OpenCode did make each change: the scenario is not empty.
		for file, marker := range map[string]string{"w.txt": "TERMA_CROSS_WRITE", "notes.txt": "TERMA_CROSS_EDIT", "sub.txt": "TERMA_CROSS_SUB", "patched.txt": "TERMA_CROSS_PATCH"} {
			if data, err := os.ReadFile(filepath.Join(other, file)); err != nil || !strings.Contains(string(data), marker) {
				t.Errorf("OpenCode did not write %s into the other checkout (%v): %q", file, err, data)
			}
		}
		// Each session was collected up to the crossing: its prompt arrived.
		var e telemetryEvidence
		if !waitUntil(45*time.Second, func() bool {
			e = sb.Receiver.evidence()
			return len(openCodeRecords(e, edited, "opencode.user_prompt")) > 0 && len(openCodeRecords(e, patched, "opencode.user_prompt")) > 0
		}) {
			t.Errorf("the sessions' prompts did not arrive: relay %v", sb.RelayStats())
		}
		// What arrives a little later would be the crossing's: give it the time.
		time.Sleep(8 * time.Second)
		e = sb.Receiver.evidence()
		if leaked := leakedFieldsOf(e, markers...); len(leaked) > 0 {
			t.Errorf("what was written into the other checkout reached upstream: %v", leaked)
		}
		for _, s := range e.spans {
			if s.Scope != openCodeScope || !strings.HasPrefix(s.Name, "execute_tool ") {
				continue
			}
			if strings.Contains(s.Attrs["gen_ai.tool.call.arguments"]+s.Attrs["opencode.tool.files"], other) {
				t.Errorf("the span of a call into the other checkout reached upstream: %s %v", s.Name, s.Attrs["gen_ai.tool.call.id"])
			}
		}
		for _, id := range []string{edited, patched} {
			if !sb.paused(id) {
				t.Errorf("terma does not hold run %s as Mixed", id)
			}
		}
		if out := sb.doctor(); !strings.Contains(out, "Paused sessions") {
			t.Errorf("terma doctor does not list the runs as paused (%s, %s):\n%s", edited, patched, out)
		}
		finishOpenCode(t, sb)
	})
}

// The real `terma setup` with OpenCode chosen and on the PATH: it writes the plugin into the
// sandbox's OpenCode plugins folder, running the terma setup ran from by its absolute path,
// tells the person to restart OpenCode, and leaves `terma doctor` nothing to report about it.
func TestOpenCodeSetup(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		sb := New(t, Isolated)
		var calls atomic.Int32
		provider := httptest.NewServer(openAIChatProvider(&calls))
		t.Cleanup(provider.Close)
		sb.UseOpenCodeProvider(provider.URL)
		out := sb.UseOpenCode(b)
		if !strings.Contains(out, "Restart OpenCode to load terma's plugin") {
			t.Errorf("setup did not say to restart OpenCode:\n%s", out)
		}
		src, err := os.ReadFile(filepath.Join(sb.Home, ".config", "opencode", "plugins", "terma.js"))
		if err != nil {
			t.Fatalf("setup wrote no plugin: %v", err)
		}
		line, ok := "", false
		for l := range strings.Lines(string(src)) {
			if rest, found := strings.CutPrefix(l, "const CONFIG = "); found {
				line, ok = strings.TrimSpace(rest), true
			}
		}
		var cfg struct {
			Endpoint    string   `json:"endpoint"`
			HookCommand []string `json:"hookCommand"`
		}
		if !ok || json.Unmarshal([]byte(line), &cfg) != nil {
			t.Fatalf("the plugin's CONFIG is not setup's: %q", line)
		}
		// Setup runs from the sandbox's terma on the PATH (Setup), and records it as it ran.
		shim := filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "terma")
		if len(cfg.HookCommand) != 2 || cfg.HookCommand[0] != shim || cfg.HookCommand[1] != "hook" {
			t.Errorf("the plugin runs %q, want %s hook", cfg.HookCommand, shim)
		}
		if !strings.Contains(cfg.Endpoint, sb.relayAddr) {
			t.Errorf("the plugin exports to %q, not the relay on %s", cfg.Endpoint, sb.relayAddr)
		}
		// Doctor names an agent only where it finds a problem (TestOpenCodeTermaRemoved): with
		// OpenCode chosen and on the PATH, nothing fails or warns but the sandbox's own recording
		// terma, which every sandbox puts first on the PATH, and the agents export.
		doctor := sb.doctor()
		exporting := false
		for l := range strings.Lines(doctor) {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			check := strings.Join(f[1:min(len(f), 5)], " ")
			exporting = exporting || f[0] == "✓" && strings.HasPrefix(check, "agent exporting to Terma")
			if (f[0] == "✗" || f[0] == "!") && !strings.HasPrefix(check, "terma on PATH") {
				t.Errorf("terma doctor reports a problem: %s", strings.TrimSpace(l))
			}
		}
		if !exporting || t.Failed() {
			t.Errorf("terma doctor:\n%s", doctor)
		}
	})
}

// One OpenCode 2 background service holds sessions in several directories, and every
// directory's copy of the plugin receives every event: each session still arrives once,
// under its own repository, and a session in a folder no team collects arrives not at all.
// The service outlives each run, so the test stops it, and fails if it is left running.
func TestOpenCodeBackgroundService(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		if !openCode2(b) {
			t.Skip("OpenCode 1 has no background service")
		}
		track(t)
		const file = "TERMA_SERVICE_WRITE.txt"
		sb := New(t, Isolated, WithRepositories("github.com/acme/repo", "github.com/acme/other"))
		other := sb.newRepo("other", "https://github.com/acme/other.git")
		stray := sb.newRepo("stray", "https://github.com/someone/stray.git")
		write := openCodeCallFor(b, "call_w", openCodeIntent{writePath: filepath.Join(other, file), content: "TERMA_SERVICE\n"})
		// The first run asks for no tool; the second's first request writes the file.
		openCodeRelayIn(sb, b, true, func(*Sandbox) []openCodeStep {
			return []openCodeStep{{text: "TERMA_TELEMETRY_REPLY"}, {calls: []openCodeCall{write}}}
		})

		service := func(verb string) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, b.Path, "service", verb)
			cmd.Dir, cmd.Env, cmd.WaitDelay = sb.Repo, sb.termaEnv(), 5*time.Second
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("opencode service %s: %v\n%s", verb, err, out)
			}
		}
		// Registered before the service starts, so a failure anywhere below still stops it.
		t.Cleanup(func() {
			service("stop")
			if !waitUntil(15*time.Second, func() bool { return len(sb.openCodeProcesses(b)) == 0 }) {
				t.Errorf("OpenCode processes of the sandbox outlived `opencode service stop`: %v", sb.openCodeProcesses(b))
			}
			if !waitUntil(10*time.Second, func() bool { return len(sb.termaHookProcesses()) == 0 }) {
				t.Errorf("terma hook processes of the sandbox outlived `opencode service stop`: %v", sb.termaHookProcesses())
			}
		})
		sb.requireOpenCodeSandbox(b)
		service("start")

		first := sb.openCodeRun(b, sb.Repo, "", "First. TERMA_OPENCODE_PROMPT", nil, true, nil)
		second := sb.openCodeRun(b, other, "", "Write it. TERMA_OPENCODE_PROMPT", nil, true, nil)
		third := sb.openCodeRun(b, stray, "", "Stray. TERMA_STRAY_WORK", nil, true, nil)
		if first == second || second == third || first == third {
			t.Fatalf("the service ran sessions %s, %s, %s: want three", first, second, third)
		}

		var e telemetryEvidence
		if !waitUntil(45*time.Second, func() bool {
			e = sb.Receiver.evidence()
			return len(openCodeCalls(e, first)) > 0 && len(openCodeCalls(e, second)) >= 2 &&
				len(termaDelivered(e, "terma.files.touched", second)) > 0 &&
				len(termaDelivered(e, "terma.session.start", first)) > 0 && len(termaDelivered(e, "terma.session.start", second)) > 0
		}) {
			t.Fatalf("the service's sessions did not arrive whole: first %d calls, second %d calls, %d touches; relay %v",
				len(openCodeCalls(e, first)), len(openCodeCalls(e, second)), len(termaDelivered(e, "terma.files.touched", second)), sb.RelayStats())
		}
		// What arrives a little later would be a copy: give it the time.
		time.Sleep(5 * time.Second)
		e = sb.Receiver.evidence()

		ids := map[string]int{}
		for _, s := range e.spans {
			if s.Scope == openCodeScope {
				ids[fmt.Sprintf("%x", s.Proto.GetSpanId())]++
			}
		}
		for id, n := range ids {
			if n > 1 {
				t.Errorf("span %s arrived %d times", id, n)
			}
		}
		for sid, repo := range map[string]string{first: "https://github.com/acme/repo", second: "https://github.com/acme/other"} {
			calls := map[string]int{}
			for _, s := range openCodeSpans(e, sid, "") {
				key := s.Attrs["opencode.call.id"] + s.Attrs["gen_ai.tool.call.id"]
				calls[key]++
				if got := s.Resource["vcs.repository.url.full"]; got != repo {
					t.Errorf("session %s's %s arrived under %q, want %s", sid, s.Name, got, repo)
				}
			}
			for key, n := range calls {
				if n > 1 {
					t.Errorf("session %s's call %s arrived %d times", sid, key, n)
				}
			}
			// Its prompt once too, under its own repository.
			prompts := openCodeRecords(e, sid, "opencode.user_prompt")
			if len(prompts) != 1 {
				t.Errorf("session %s has %d prompt records, want one", sid, len(prompts))
			}
			for _, r := range prompts {
				if got := r.Resource["vcs.repository.url.full"]; got != repo {
					t.Errorf("session %s's prompt arrived under %q, want %s", sid, got, repo)
				}
			}
			starts := termaDelivered(e, "terma.session.start", sid)
			if len(starts) != 1 {
				t.Errorf("session %s has %d terma.session.start, want one", sid, len(starts))
			}
			for _, r := range starts {
				if got := recordRepository(r); got != repo {
					t.Errorf("session %s's terma.session.start names %q, want %s: %v %v", sid, got, repo, r.Resource, r.Attrs)
				}
			}
		}
		writes := openCodeSpans(e, second, "execute_tool "+write.name)
		touches := termaDelivered(e, "terma.files.touched", second)
		if len(writes) != 1 || len(touches) != 1 {
			t.Fatalf("%d write spans and %d touches of the second session, want one each", len(writes), len(touches))
		}
		if got, want := touches[0].Attrs["gen_ai.tool.call.id"], writes[0].Attrs["gen_ai.tool.call.id"]; got != want {
			t.Errorf("the touch names call %q, the write span %q", got, want)
		}
		if got := recordRepository(touches[0]); got != "https://github.com/acme/other" {
			t.Errorf("the touch names repository %q, want the second's: %v %v", got, touches[0].Resource, touches[0].Attrs)
		}
		if n := openCodeAgentRecords(e, third); n != 0 {
			t.Errorf("the session in a folder no team collects reached upstream: %d records", n)
		}
		for _, r := range e.logs {
			if fromTerma(r) && r.Attrs["session.id"] == third {
				t.Errorf("a hook record of the uncollected session reached upstream: %s", r.EventName)
			}
		}
		if leaked := leakedFieldsOf(e, "TERMA_STRAY_WORK"); len(leaked) > 0 {
			t.Errorf("the uncollected session's prompt reached upstream: %v", leaked)
		}
		finishOpenCode(t, sb)
	})
}

// recordRepository is the repository a record names: the relay's stamp on its resource, or
// the hook's own attribute.
func recordRepository(r LogRecord) string {
	if v := r.Resource["vcs.repository.url.full"]; v != "" {
		return v
	}
	return r.Attrs["vcs.repository.url.full"]
}

// openCodeProcesses are the running processes of b that belong to the sandbox: on Linux those
// whose executable is b and whose HOME is the sandbox's; elsewhere every process of b, found
// by its path.
func (sb *Sandbox) openCodeProcesses(b Binary) []string {
	exe, err := filepath.EvalSymlinks(b.Path)
	if err != nil {
		exe = b.Path
	}
	var out []string
	if runtime.GOOS == "linux" {
		procs, _ := filepath.Glob("/proc/[0-9]*")
		for _, p := range procs {
			if target, err := os.Readlink(filepath.Join(p, "exe")); err != nil || target != exe {
				continue
			}
			env, _ := os.ReadFile(filepath.Join(p, "environ"))
			if slices.Contains(strings.Split(string(env), "\x00"), "HOME="+sb.Home) {
				out = append(out, filepath.Base(p))
			}
		}
		return out
	}
	ps, _ := exec.Command("ps", "-axo", "pid=,comm=").Output()
	for l := range strings.Lines(string(ps)) {
		if pid, comm, ok := strings.Cut(strings.TrimSpace(l), " "); ok && strings.TrimSpace(comm) == exe {
			out = append(out, pid)
		}
	}
	return out
}

// termaHookProcesses are the `terma hook` processes running for this sandbox: on Linux those
// whose environment names its home, elsewhere every `terma hook opencode-…` (the scenarios
// run one at a time).
func (sb *Sandbox) termaHookProcesses() []string {
	var out []string
	if runtime.GOOS == "linux" {
		procs, _ := filepath.Glob("/proc/[0-9]*")
		for _, p := range procs {
			cmd, err := os.ReadFile(filepath.Join(p, "cmdline"))
			if err != nil {
				continue
			}
			args := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
			if len(args) < 3 || args[1] != "hook" || !strings.HasPrefix(args[2], "opencode-") {
				continue
			}
			env, _ := os.ReadFile(filepath.Join(p, "environ"))
			if slices.Contains(strings.Split(string(env), "\x00"), "HOME="+sb.Home) {
				out = append(out, filepath.Base(p)+" "+args[2])
			}
		}
		return out
	}
	ps, _ := exec.Command("ps", "-axo", "pid=,command=").Output()
	for l := range strings.Lines(string(ps)) {
		if f := strings.Fields(l); len(f) >= 4 && f[2] == "hook" && strings.HasPrefix(f[3], "opencode-") {
			out = append(out, f[0]+" "+f[3])
		}
	}
	return out
}

// How long the plugin's reach check takes with the real terma: the plugin holds a tool for
// it and gives it REACH_HOOK_TIMEOUT_MS. This logs the times and asserts none: it needs
// terma only, so it runs once, not on each OpenCode build. It fails only if a hook does.
func TestOpenCodeReachHookTime(t *testing.T) {
	// Set up as a person would, with the newest build on the list; OpenCode is never started.
	bins := OpenCodeBinaries(t)
	sb := openCodeRelay(t, bins[len(bins)-1], true, nil)
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
