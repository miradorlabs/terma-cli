package e2e

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ompSandbox(t *testing.T, cmd string) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"omp"}
		var calls atomic.Int32
		// omp asks for a session title first, offering no tools: the call goes to the first
		// request that offers them.
		tool, args := "", map[string]any(nil)
		if cmd != "" {
			tool, args = "bash", map[string]any{"command": cmd, "description": "workload"}
		}
		provider := httptest.NewServer(openAIProbeProvider(&calls, tool, args))
		t.Cleanup(provider.Close)
		sb.UseOmpProvider(provider.URL)
		return sb
	}
}

// omp through the relay: its native exporter sends the same telemetry as it does pointed
// straight at the receiver, and the relay drops none of an opted-in session's.
func TestRelayWorkloadsOmp(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.equivalent")
		for _, w := range []struct{ name, cmd string }{{"reply", ""}, {"bash", "printf ok"}} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, b, ompSandbox(t, w.cmd), func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.UseOmpNativeDirect()
					}
					sb.OmpRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// omp outside any admitted repository: its native exporter sends to the relay, the relay holds
// the unclaimed session and drops it, and nothing reaches upstream.
func TestRelayOmpOutsideARepository(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.only_opted_in")
		track(t)
		sb := ompSandbox(t, "")(t)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.OmpRun(b, personal, "TERMA_PERSONAL_WORK")
		time.Sleep(6 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("omp outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from omp, so the control proves nothing: %v", c)
		}
	})
}

// Nothing omp runs inherits terma's endpoint or its key: omp keeps the OTEL_* of its .env
// from the tools it runs (see TestRelayClaudeToolsGetNoExporter for why it matters).
func TestRelayOmpToolsGetNoExporter(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.tools_no_token")
		track(t)
		out, err := os.CreateTemp("", "terma-omp-env-*")
		if err != nil {
			t.Fatal(err)
		}
		_ = out.Close()
		t.Cleanup(func() { _ = os.Remove(out.Name()) })
		sb := ompSandbox(t, "env | cut -d= -f1 | grep '^OTEL_' > "+out.Name()+"; printf ran >> "+out.Name())(t)
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		sb.OmpRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
		time.Sleep(6 * time.Second)
		data, _ := os.ReadFile(out.Name())
		if !strings.Contains(string(data), "ran") {
			t.Fatalf("omp's bash tool never ran: %q", data)
		}
		if names := strings.Fields(strings.ReplaceAll(string(data), "ran", "")); len(names) > 0 {
			t.Errorf("omp's tools inherited exporter settings: %v", names)
		}
		if agentRecords(sb.Receiver.evidence()) == 0 {
			t.Errorf("omp's session reached nothing upstream: %v", sb.RelayStats())
		}
		sb.StopRelay()
	})
}

// A file omp writes is attributed: terma's extension hands the edit to omp-file-edit, naming
// the tool call as omp's spans do, and the commit of it is stamped with omp's session.
func TestOmpAttribution(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		track(t)
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"omp"}
		var calls atomic.Int32
		provider := httptest.NewServer(openAIProbeProvider(&calls, "write", map[string]any{"path": "hello.txt", "content": "hello\n"}))
		t.Cleanup(provider.Close)
		sb.UseOmpProvider(provider.URL)
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		sb.OmpRun(b, sb.Repo, "Write hello.txt.")
		starts := sb.WaitEvents("terma.session.start", "", 10*time.Second)
		if len(starts) == 0 {
			t.Fatalf("omp announced no session; spool: %+v", sb.Spool())
		}
		sid := starts[0].SessionID
		evs := sb.WaitEvents("terma.files.touched", sid, 10*time.Second)
		if len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; spool: %+v", sid, sb.Spool())
		}
		if files := listAttr(evs[0].Attrs["terma.files.paths"]); !slices.Contains(files, "hello.txt") {
			t.Errorf("files touched = %q", files)
		}
		if id, _ := evs[0].Attrs["gen_ai.tool.call.id"].(string); id == "" {
			t.Errorf("the edit names no tool call: %+v", evs[0].Attrs)
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) || !strings.Contains(msg, "Agent-Tool: omp") {
			t.Errorf("commit not stamped with omp's session %s:\n%s", sid, msg)
		}
	})
}

// terma asks OMP for all content because the relay applies the team's policy to it, and only
// where it goes to the relay: a traces endpoint the developer's shell, repository .env or
// agent .env sets gets no content on terma's request, OMP running terma's extension or not
// (--no-extensions); with every signal on the relay the relay receives it all.
func TestRelayOmpContentOnlyToTheRelay(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		track(t)
		const marker = "TERMA_OMP_PRIVATE_PROMPT"
		promptIn := func(e telemetryEvidence) []string {
			return slices.DeleteFunc(leakedFieldsOf(e, marker), func(f string) bool { return !strings.Contains(f, "gen_ai") })
		}
		for _, override := range []string{"shell", "repository .env"} {
			for _, flags := range [][]string{nil, {"--no-extensions"}} {
				t.Run(strings.TrimSpace(override+" "+strings.Join(flags, " ")), func(t *testing.T) {
					sb := ompSandbox(t, "")(t)
					other := StartReceiver(t)
					env := []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + other.URL() + "/v1/traces", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf"}
					if override == "shell" {
						sb.ExtraEnv = append(sb.ExtraEnv, env...)
					} else {
						sb.writeAbs(filepath.Join(sb.Repo, ".env"), strings.Join(env, "\n")+"\n")
					}
					sb.UseRelay(RelayOptions{Start: true, Content: true})
					sb.OmpRun(b, sb.Repo, "Reply to "+marker, flags...)
					for deadline := time.Now().Add(6 * time.Second); len(other.evidence().spans) == 0 && time.Now().Before(deadline); {
						time.Sleep(40 * time.Millisecond)
					}
					ev := other.evidence()
					if len(ev.spans) == 0 {
						t.Fatal("the overriding endpoint received no traces, so the check proves nothing")
					}
					if fields := promptIn(ev); len(fields) > 0 {
						t.Errorf("the prompt left for the developer's own endpoint: %v", fields)
					}
					sb.StopRelay()
				})
			}
		}
		// The agent .env pointed elsewhere after setup, or never pointed at the relay (a setup
		// that wrote the extension and failed at the .env): the extension asks for nothing.
		agentEnv := map[string]func(sb *Sandbox, other string){
			"agent .env edited": func(sb *Sandbox, other string) {
				path := filepath.Join(sb.Home, ".omp", "agent", ".env")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var lines []string
				for l := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
					if strings.HasPrefix(l, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=") {
						l = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + other + "/v1/traces"
					}
					lines = append(lines, l)
				}
				sb.writeAbs(path, strings.Join(lines, "\n")+"\n")
			},
			"agent .env never pointed": func(sb *Sandbox, other string) {
				sb.writeAbs(filepath.Join(sb.Home, ".omp", "agent", ".env"), "OTEL_EXPORTER_OTLP_ENDPOINT="+other+"\nOTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf\n")
			},
		}
		for name, edit := range agentEnv {
			t.Run(name, func(t *testing.T) {
				sb := ompSandbox(t, "")(t)
				other := StartReceiver(t)
				sb.UseRelay(RelayOptions{Start: true, Content: true})
				edit(sb, other.URL())
				sb.OmpRun(b, sb.Repo, "Reply to "+marker)
				for deadline := time.Now().Add(6 * time.Second); len(other.evidence().spans) == 0 && time.Now().Before(deadline); {
					time.Sleep(40 * time.Millisecond)
				}
				ev := other.evidence()
				if len(ev.spans) == 0 {
					t.Fatal("the developer's endpoint received no traces, so the check proves nothing")
				}
				if fields := promptIn(ev); len(fields) > 0 {
					t.Errorf("the prompt left for the developer's own endpoint: %v", fields)
				}
				sb.StopRelay()
			})
		}
		t.Run("none", func(t *testing.T) {
			ProvesAll(t, b, "relay.content_allowed")
			sb := ompSandbox(t, "")(t)
			sb.UseRelay(RelayOptions{Start: true, Content: true})
			sb.OmpRun(b, sb.Repo, "Reply to "+marker)
			for deadline := time.Now().Add(10 * time.Second); len(promptIn(sb.Receiver.evidence())) == 0 && time.Now().Before(deadline); {
				time.Sleep(100 * time.Millisecond)
			}
			if len(promptIn(sb.Receiver.evidence())) == 0 {
				t.Errorf("the relay passed on no prompt though the policy allows it: %v", sb.RelayStats())
			}
			sb.StopRelay()
		})
	})
}
