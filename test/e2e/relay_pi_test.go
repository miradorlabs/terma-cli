package e2e

import (
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

// Pi through the real `terma setup --harness pi` and the relay. The scenarios Pi can only
// reach through its terminal app or an RPC session (/compact, /resume, /reload, /clone,
// queued prompts) are proved on Pi's own recorded events by the extension's replay tests in
// internal/agents/pi/extension_test.go, each named below where it stands in for a scenario.

// piToolSandbox is an admitted repository whose Pi is asked for one call of tool.
func piToolSandbox(tool string, args map[string]any) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"pi"}
		var calls atomic.Int32
		provider := httptest.NewServer(openAIToolCallProvider(&calls, tool, args))
		t.Cleanup(provider.Close)
		sb.UsePiProvider(provider.URL)
		return sb
	}
}

// piSpans and piRecords are what reached upstream of session under terma's Pi scope; ""
// is every session.
func piSpans(e telemetryEvidence, session string) []Span {
	var out []Span
	for _, s := range e.spans {
		if s.Scope == piScope && (session == "" || s.Attrs["session.id"] == session) {
			out = append(out, s)
		}
	}
	return out
}

func piRecords(e telemetryEvidence, session, event string) []LogRecord {
	var out []LogRecord
	for _, r := range e.logs {
		if r.Scope == piScope && (session == "" || r.Attrs["session.id"] == session) && (event == "" || r.Attrs["event.name"] == event) {
			out = append(out, r)
		}
	}
	return out
}

// waitPi waits until upstream holds a span of every session, then a little longer for what
// follows it, and returns what arrived.
func (sb *Sandbox) waitPi(sessions ...string) telemetryEvidence {
	sb.T.Helper()
	var e telemetryEvidence
	if !waitUntil(45*time.Second, func() bool {
		e = sb.Receiver.evidence()
		for _, s := range sessions {
			if len(piSpans(e, s)) == 0 {
				return false
			}
		}
		return true
	}) {
		sb.T.Errorf("sessions %v did not all reach upstream: relay %v", sessions, sb.RelayStats())
	}
	time.Sleep(3 * time.Second)
	return sb.Receiver.evidence()
}

func piTool(spans []Span, name string) (Span, bool) {
	i := slices.IndexFunc(spans, func(s Span) bool { return s.Attrs["gen_ai.tool.name"] == name })
	if i < 0 {
		return Span{}, false
	}
	return spans[i], true
}

// Pi through the relay: terma's extension exports the same spans and logs as it does
// pointed straight at the receiver, and the relay drops none of an opted-in session's.
func TestRelayWorkloadsPi(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.equivalent")
		for _, w := range []struct {
			name, tool string
			args       map[string]any
		}{
			{"reply", "", nil},
			{"bash", "bash", map[string]any{"command": "printf ok"}},
			{"write", "write", map[string]any{"path": "hello.txt", "content": "hello\n"}},
		} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, b, func(t *testing.T) *Sandbox {
					sb := piToolSandbox(w.tool, w.args)(t)
					sb.UsePi(b)
					return sb
				}, func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.UsePiExtensionDirect()
					}
					sb.PiRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// One prompt with a write, an edit, a failing command and a tool Pi does not have, under
// each team policy: the calls and their outcomes leave whatever the policy says, with their
// turn; the prompt and the reply follow the prompt switch; arguments, results and files
// follow tool content; and the commit of the write is credited to the session.
func TestRelayPi(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.telemetry")
		for _, pol := range []struct {
			name           string
			o              RelayOptions
			prompts, tools bool
		}{
			{"everything", RelayOptions{Content: true}, true, true},
			{"prompts withheld", RelayOptions{Withhold: "prompts"}, false, true},
			{"tool content withheld", RelayOptions{Withhold: "tool content"}, true, false},
			{"both withheld", RelayOptions{}, false, false},
		} {
			t.Run(pol.name, func(t *testing.T) {
				Proves(t, b.Harness, b.Version, map[bool]string{true: "relay.content_allowed", false: "relay.content_withheld"}[pol.prompts && pol.tools])
				track(t)
				sb := New(t, Isolated)
				sb.writeAbs(filepath.Join(sb.Repo, "notes.txt"), "alpha\nbeta\n")
				piRelayIn(sb, b, pol.o, openCodeConversation{key: "TERMA_PI_PROMPT", steps: []openCodeStep{
					piCall("call_w", "write", map[string]any{"path": "new.txt", "content": "TERMA_PI_WRITE\n"}),
					piCall("call_e", "edit", map[string]any{"path": "notes.txt", "edits": []any{map[string]any{"oldText": "beta", "newText": "TERMA_PI_EDIT"}}}),
					piCall("call_b", "bash", map[string]any{"command": "printf TERMA_PI_TOOL_OUTPUT; exit 3"}),
					piCall("call_u", "no_such_tool", map[string]any{}),
				}})
				sid := sb.PiSession(b, sb.Repo, "Make the changes. TERMA_PI_PROMPT")
				e := sb.waitPi(sid)
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
				checkExportRequests(&failures, e, "/v1/logs", "/v1/traces")
				for _, f := range failures {
					t.Error(f)
				}
				spans := piSpans(e, sid)
				for tool, outcome := range map[string]string{"write": "ok", "edit": "ok", "bash": "error", "no_such_tool": "not_run"} {
					s, ok := piTool(spans, tool)
					if !ok || s.Attrs["pi.tool.outcome"] != outcome || s.Attrs["pi.turn.id"] == "" || s.Attrs["pi.call.id"] == "" {
						t.Errorf("%s span %v (found %v), want outcome %s with its turn and call", tool, s.Attrs, ok, outcome)
					}
					if has := s.Attrs["gen_ai.tool.call.arguments"] != ""; ok && has != pol.tools {
						t.Errorf("%s arguments arrived %v under tool content %v", tool, has, pol.tools)
					}
				}
				if s, _ := piTool(spans, "bash"); s.Attrs["pi.tool.exit_code"] != "3" {
					t.Errorf("bash exit code %q", s.Attrs["pi.tool.exit_code"])
				}
				if s, _ := piTool(spans, "write"); (s.Attrs["pi.tool.files"] != "") != pol.tools {
					t.Errorf("the write's files %q under tool content %v", s.Attrs["pi.tool.files"], pol.tools)
				}
				prompts := piRecords(e, sid, "pi.user_prompt")
				if len(prompts) != 1 || strings.Contains(prompts[0].Body, "TERMA_PI_PROMPT") != pol.prompts || prompts[0].Attrs["pi.turn.id"] == "" {
					t.Errorf("prompt records %+v under prompts %v", prompts, pol.prompts)
				}
				if leaked := leakedFieldsOf(e, "TERMA_TELEMETRY_REPLY"); (len(leaked) > 0) != pol.prompts {
					t.Errorf("the reply arrived in %v under prompts %v", leaked, pol.prompts)
				}
				if leaked := leakedFieldsOf(e, "TERMA_PI_TOOL_OUTPUT", "TERMA_PI_WRITE"); !pol.tools && len(leaked) > 0 {
					t.Errorf("tool content withheld, but it arrived in %v", leaked)
				}
				msg := sb.Commit("add new.txt")
				if !strings.Contains(msg, "Agent-Session-Id: "+sid) || !strings.Contains(msg, "Agent-Tool: pi") {
					t.Errorf("commit not credited to Pi's session %s:\n%s", sid, msg)
				}
				sb.StopRelay()
				c := sb.RelayStats()
				noteRelayStats(t.Name(), c)
				failUnclassified(t, c)
				if n := sum(c, "dropped."); n > 0 {
					t.Errorf("the relay dropped %d records of an opted-in session: %v", n, c)
				}
			})
		}
	})
}

// A resumed run in a new process is the same session and sends nothing twice; a fork is a
// new session naming its source and sends nothing it copied.
func TestRelayPiResumeAndFork(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		track(t)
		sb := piRelayIn(New(t, Isolated), b, RelayOptions{Content: true})
		first := sb.PiSession(b, sb.Repo, "TERMA_RUN_ONE")
		if again := sb.PiSession(b, sb.Repo, "TERMA_RUN_TWO", "-c"); again != first {
			t.Fatalf("pi -c ran session %s, want %s", again, first)
		}
		fork := sb.PiSession(b, sb.Repo, "TERMA_RUN_FORK", "--fork", first)
		if fork == first {
			t.Fatal("the fork kept its source's session")
		}
		e := sb.waitPi(first, fork)
		seen := map[string]bool{}
		for _, s := range piSpans(e, "") {
			id := s.Attrs["session.id"] + "/" + s.Attrs["pi.call.id"] + "/" + s.Attrs["gen_ai.tool.call.id"]
			if seen[id] {
				t.Errorf("sent twice: %s", id)
			}
			seen[id] = true
			if src := s.Attrs["pi.session.forked_from.id"]; (s.Attrs["session.id"] == fork) != (src == first) {
				t.Errorf("span of %s names source %q", s.Attrs["session.id"], src)
			}
		}
		for _, r := range piRecords(e, fork, "pi.user_prompt") {
			if !strings.Contains(r.Body, "TERMA_RUN_FORK") {
				t.Errorf("the fork sent a prompt it copied: %q", r.Body)
			}
		}
		if n := len(piRecords(e, first, "pi.user_prompt")); n != 2 {
			t.Errorf("the resumed session sent %d prompts, want 2", n)
		}
	})
}

// A provider error Pi retries: the failed attempt is a call of its own, status error, in
// the same turn as the answer.
func TestRelayPiRetry(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		track(t)
		sb := New(t, Isolated)
		sb.UsePiScript(1)
		sb.UsePi(b)
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		sid := sb.PiSession(b, sb.Repo, "TERMA_RETRY")
		var failed, answered int
		turns := map[string]bool{}
		for _, s := range piSpans(sb.waitPi(sid), sid) {
			if !strings.HasPrefix(s.Name, "chat ") {
				continue
			}
			turns[s.Attrs["pi.turn.id"]] = true
			if s.Proto.GetStatus().GetCode() == 2 && s.Attrs["gen_ai.response.finish_reasons"] == "error" {
				failed++
			} else {
				answered++
			}
		}
		if failed != 1 || answered != 1 || len(turns) != 1 || turns[""] {
			t.Errorf("calls: %d failed, %d answered, turns %v", failed, answered, turns)
		}
	})
}

// A child pi a bash call starts works for the session that started it; one whose parent
// shell has exited before it starts is reparented, and is a session of its own.
func TestRelayPiChildren(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		track(t)
		const flags = "pi -p --model fake/m --offline --no-context-files --no-skills"
		sb := piRelayIn(New(t, Isolated), b, RelayOptions{Content: true},
			openCodeConversation{key: "TERMA_PARENT", steps: []openCodeStep{
				piCall("call_linked", "bash", map[string]any{"command": flags + " TERMA_CHILD_LINKED"}),
				piCall("call_orphan", "bash", map[string]any{"command": "(sleep 2; " + flags + " TERMA_CHILD_ORPHAN) >/dev/null 2>&1 &"}),
			}})
		// The children announce their sessions too, so the parent is the session of its prompt.
		sb.PiRun(b, sb.Repo, "TERMA_PARENT")
		var e telemetryEvidence
		waitUntil(60*time.Second, func() bool {
			e = sb.Receiver.evidence()
			return len(piRecords(e, "", "pi.user_prompt")) >= 3
		})
		parent := ""
		for _, r := range piRecords(e, "", "pi.user_prompt") {
			if strings.Contains(r.Body, "TERMA_PARENT") {
				parent = r.Attrs["session.id"]
			}
		}
		if parent == "" {
			t.Fatalf("the parent's prompt did not arrive: relay %v", sb.RelayStats())
		}
		e = sb.waitPi(parent)
		for _, r := range piRecords(e, "", "pi.user_prompt") {
			linked := r.Attrs["pi.parent_session.id"]
			switch {
			case strings.Contains(r.Body, "TERMA_CHILD_LINKED") && linked != parent:
				t.Errorf("the child a bash call started names parent %q, want %s", linked, parent)
			case strings.Contains(r.Body, "TERMA_CHILD_ORPHAN") && linked != "":
				t.Errorf("a reparented child names parent %q", linked)
			case strings.Contains(r.Body, "TERMA_PARENT") && linked != "":
				t.Errorf("the parent names parent %q", linked)
			}
		}
		for _, k := range []string{"TERMA_CHILD_LINKED", "TERMA_CHILD_ORPHAN"} {
			if !slices.ContainsFunc(piRecords(e, "", "pi.user_prompt"), func(r LogRecord) bool { return strings.Contains(r.Body, k) }) {
				t.Errorf("no session ran %s", k)
			}
		}
	})
}

// A read into another organization's checkout, named as Pi names paths: absolute, a file
// URL, @ before a file URL, ~, and a grep whose path starts ~/ (grep is opt-in: --tools);
// an edit whose text is not there, a write, and a file attached on the command line. Each
// is judged as the place Pi opens, the run goes Mixed at the check, and nothing of the
// other checkout leaves: no tool span, no attached prompt, none of the file.
func TestRelayPiReadIntoAnotherCheckout(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		const otherOrigin = "github.com/someone/other"
		sb := New(t, Isolated, WithOtherOrganization(otherOrigin))
		other := filepath.Join(sb.Home, "other")
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.gitIn(other, "init", "-q", "-b", "main")
		sb.gitIn(other, "remote", "add", "origin", "https://"+otherOrigin+".git")
		secret := filepath.Join(other, "secret.txt")
		sb.writeAbs(secret, "TERMA_CROSS_SECRET\n")
		type form struct {
			step  *openCodeStep
			flags []string
		}
		call := func(id, name string, args map[string]any, flags ...string) form {
			step := piCall(id, name, args)
			return form{&step, flags}
		}
		forms := map[string]form{
			"TERMA_FORM_ABS":     call("c1", "read", map[string]any{"path": secret}),
			"TERMA_FORM_URL":     call("c2", "read", map[string]any{"path": "file://" + secret}),
			"TERMA_FORM_AT_URL":  call("c3", "read", map[string]any{"path": "@file://" + secret}),
			"TERMA_FORM_TILDE":   call("c4", "read", map[string]any{"path": "~/other/secret.txt"}),
			"TERMA_FORM_GREP_TD": call("c5", "grep", map[string]any{"pattern": "TERMA_CROSS", "path": "~/other"}, "--tools", "read,grep"),
			"TERMA_FORM_EDIT":    call("c6", "edit", map[string]any{"path": secret, "edits": []any{map[string]any{"oldText": "NOT THERE", "newText": "x"}}}),
			"TERMA_FORM_WRITE":   call("c7", "write", map[string]any{"path": filepath.Join(other, "new.txt"), "content": "TERMA_CROSS_WRITE\n"}),
			"TERMA_FORM_ATTACH":  {flags: []string{"@" + secret}},
		}
		var convs []openCodeConversation
		for key, f := range forms {
			if f.step != nil {
				convs = append(convs, openCodeConversation{key: key, steps: []openCodeStep{*f.step}})
			}
		}
		track(t)
		piRelayIn(sb, b, RelayOptions{Content: true}, convs...)
		teams := exec.Command(sb.Terma, "teams", "--select", otherOrgProject)
		teams.Dir, teams.Env = sb.Repo, sb.termaEnv()
		if out, err := teams.CombinedOutput(); err != nil {
			t.Fatalf("terma teams: %v\n%s", err, out)
		}
		sessions := map[string]string{}
		for key, f := range forms {
			sessions[key] = sb.PiSession(b, sb.Repo, "Read it. "+key, f.flags...)
		}
		time.Sleep(8 * time.Second)
		e := sb.Receiver.evidence()
		if n := len(piRecords(e, sessions["TERMA_FORM_ATTACH"], "pi.user_prompt")); n > 0 {
			t.Errorf("the prompt with another checkout's file attached reached upstream")
		}
		if leaked := leakedFieldsOf(e, "TERMA_CROSS_SECRET", "TERMA_CROSS_WRITE"); len(leaked) > 0 {
			t.Errorf("what the reads returned reached upstream: %v", leaked)
		}
		for key, sid := range sessions {
			if !sb.paused(sid) {
				t.Errorf("%s: terma does not hold run %s as Mixed", key, sid)
			}
			for _, s := range piSpans(e, sid) {
				if strings.HasPrefix(s.Name, "execute_tool ") {
					t.Errorf("%s: the read's span reached upstream: %v", key, s.Attrs["gen_ai.tool.call.id"])
				}
			}
		}
	})
}

// A message an extension returns from before_agent_start: the prompt and the custom message
// both arrive, both in the prompt's turn.
func TestRelayPiInjectedMessage(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		track(t)
		sb := New(t, Isolated)
		sb.writeAbs(filepath.Join(sb.piAgentDir(), "extensions", "inject.ts"),
			`export default function (pi: any) { pi.on("before_agent_start", () => ({ message: { customType: "e2e", content: "TERMA_INJECTED_CONTEXT", display: true } })) }`+"\n")
		piRelayIn(sb, b, RelayOptions{Content: true})
		sid := sb.PiSession(b, sb.Repo, "TERMA_INJECT_PROMPT")
		e := sb.waitPi(sid)
		prompts, customs := piRecords(e, sid, "pi.user_prompt"), piRecords(e, sid, "pi.custom_message")
		if len(prompts) != 1 || len(customs) != 1 {
			t.Fatalf("prompts %d, custom messages %d", len(prompts), len(customs))
		}
		if customs[0].Body != "TERMA_INJECTED_CONTEXT" || customs[0].Attrs["pi.turn.id"] != prompts[0].Attrs["pi.turn.id"] {
			t.Errorf("custom message %q in turn %q, prompt's turn %q", customs[0].Body, customs[0].Attrs["pi.turn.id"], prompts[0].Attrs["pi.turn.id"])
		}
	})
}

// Pi outside any admitted repository, and in a repository no team collects: its extension
// exports to the relay, the relay holds the unclaimed session and drops it, and nothing
// reaches upstream.
func TestRelayPiOutsideARepository(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.only_opted_in")
		for _, place := range []string{"no repository", "uncollected repository"} {
			t.Run(place, func(t *testing.T) {
				track(t)
				sb := piToolSandbox("", nil)(t)
				sb.UsePi(b)
				sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
				dir := filepath.Join(sb.Dir, "personal")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if place == "uncollected repository" {
					sb.gitIn(dir, "init", "-q", "-b", "main")
					sb.gitIn(dir, "remote", "add", "origin", "https://github.com/someone/uncollected.git")
				}
				sb.PiRun(b, dir, "TERMA_PERSONAL_WORK")
				time.Sleep(8 * time.Second)
				sb.StopRelay()
				c := sb.RelayStats()
				noteRelayStats(t.Name(), c)
				failUnclassified(t, c)
				if n := agentRecords(sb.Receiver.evidence()); n != 0 {
					t.Errorf("Pi in %s reached upstream: %d records, relay %v", place, n, c)
				}
				if sum(c, "received.") == 0 {
					t.Errorf("the relay received nothing from Pi, so the control proves nothing: %v", c)
				}
			})
		}
	})
}

// A Pi below 1.5.0 is refused by setup, which writes nothing. (That a hand-placed extension
// stays inert in such a Pi is proved by TestReplayHostCheck.)
func TestPiBelowTheMinimum(t *testing.T) {
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	skipUnlessSelected(t, "pi")
	sb := New(t, Isolated)
	sb.writeAbs(filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "pi"), "#!/bin/sh\necho 1.4.9\n")
	if err := os.Chmod(filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := sb.trySetup(nil, "--team", sb.ProjectID, "--harness", "pi")
	if err == nil || !strings.Contains(out, "Pi 1.5.0 or later is required (found 1.4.9)") {
		t.Errorf("setup of Pi 1.4.9: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(sb.piAgentDir(), "extensions", "terma.ts")); !os.IsNotExist(err) {
		t.Errorf("setup wrote the extension of a Pi it refused")
	}
}

// An extension an earlier terma wrote (wire version 1) is rewritten in place by `terma
// update --refresh`, keeping where it sends, and Pi then exports version 2.
func TestPiExtensionRefreshedInPlace(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		track(t)
		sb := piRelayIn(New(t, Isolated), b, RelayOptions{Content: true})
		path := filepath.Join(sb.piAgentDir(), "extensions", "terma.ts")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, _ := strings.Cut(string(data), "const CONFIG: TermaConfig | null = ")
		line, _, _ := strings.Cut(rest, "\n")
		sb.writeAbs(path, "// terma's Pi extension, wire version 1\nconst CONFIG: TermaConfig | null = "+line+"\nexport default function (pi: any) {}\n")
		sb.terma(sb.Repo, "update", "--refresh")
		if refreshed, _ := os.ReadFile(path); string(refreshed) != string(data) {
			t.Fatalf("the refreshed extension is not this build's for the same configuration")
		}
		sid := sb.PiSession(b, sb.Repo, "TERMA_REFRESHED")
		for _, s := range piSpans(sb.waitPi(sid), sid) {
			if s.ScopeVersion != "2" {
				t.Errorf("span %s under scope version %q", s.Name, s.ScopeVersion)
			}
		}
	})
}

// Covered by the replay tests rather than a live Pi, each needing the terminal app or an RPC
// session this suite does not drive:
//   - a compaction, and two with the same summary: TestReplayCompactionThreshold,
//     TestReplayTwoCompactions, TestReplayCompactionsWithOneSummary, TestReplayDraftCompaction;
//   - /resume back into a session another process wrote to: TestReplayResumeIntoChangedSession;
//   - the reach hook failing (Mixed, no wait at the next call, /reload keeps the fence):
//     TestReplayReload and TestPiToolReachFailsClosedUnderNode;
//   - /reload and then a commit (one start, one end: the session stays the active one):
//     TestReplayReload;
//   - two prompts queued and delivered together: TestReplayCustomMessages/21-queued-pair;
//   - a clone of a session that ends in a custom message: TestReplayCustomMessages/22-clone-after-custom;
//   - a call that did not run for each reason (blocked, unknown, invalid): TestReplayToolOutcomes
//     (an unknown tool is also in TestRelayPi).
