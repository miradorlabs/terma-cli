package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/desktoprelay"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const (
	replySession = "01a0bae3-e62f-72d3-99e7-355f3c7d5fba"
	replyTurn    = "01a0bae4-4a41-7910-9022-15897b3021ae"
	replyTrace   = "ae2a5e6f8e0168ae5ec2efa2c8772b02"
)

// replyRollout writes a one-turn rollout in Codex 0.155.1's shapes and returns its path.
func replyRollout(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+replySession+".jsonl")
	writeFile(t, filepath.Dir(path), filepath.Base(path), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + replySession + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39.200Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET PROMPT the developer typed"}]}}`,
		`{"timestamp":"2026-09-19T18:18:41.265Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_a1","phase":"commentary","content":[{"type":"output_text","text":"I'll check the command list."}]}}`,
		`{"timestamp":"2026-09-19T18:19:47.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_a2","phase":"final_answer","content":[{"type":"output_text","text":"Three of them can go."}]}}`,
		`{"timestamp":"2026-09-19T18:19:48.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurn + `"}}`,
	}, "\n")+"\n")
	return path
}

func routeCodex(t *testing.T, includePrompts bool) {
	t.Helper()
	t.Setenv(shim.CodexRoutedEnv, "1")
	if err := shim.SaveRecord(shim.Record{ProjectID: "project-a", Endpoint: "https://otel.terma.ai", Signals: []string{"logs"},
		IncludePrompts: includePrompts, Harnesses: []string{shim.AgentCodex}}); err != nil {
		t.Fatal(err)
	}
}

func connectCodexMachineWide(t *testing.T, includePrompts bool) {
	t.Helper()
	err := (harness.Codex{}).Connect(harness.Exporter{
		Endpoint: "https://otel.terma.ai", APIKey: "ter_srv_0123456789abcdef01234567", ProjectID: "project-a",
		Signals: []harness.Signal{harness.SignalLogs}, IncludePrompts: includePrompts,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := (harness.Codex{}).Status(); err != nil || !st.Connected || st.IncludePrompts != includePrompts {
		t.Fatalf("machine-wide connect did not take: %+v %v", st, err)
	}
}

func connectCodexDesktop(t *testing.T, includePrompts bool) {
	t.Helper()
	if err := (harness.Codex{}).Connect(harness.Exporter{
		Endpoint: desktoprelay.Endpoint, Signals: []harness.Signal{harness.SignalLogs},
		IncludePrompts: includePrompts,
	}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shim.CodexRoutedEnv, "")
}

func stopCodex(t *testing.T, env Env, path string) []spool.Event {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-5.6-luna"})
	env.Stdin = strings.NewReader(string(b))
	if err := CodexStop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	var replies []spool.Event
	for _, e := range spooled(t, env.Spool) {
		if e.Name == EventAssistantMessage {
			replies = append(replies, e)
		}
	}
	return replies
}

// Codex exports what the developer said and what its tools did, never what it answered.
// For a developer whose Codex exports their prompts, the end of a turn spools the replies
// — in the turn the platform knows by its trace id, stamped with when they were said.
func TestCodexStopSpoolsWhatCodexSaid(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, true)
	path := replyRollout(t)

	replies := stopCodex(t, env, path)
	if len(replies) != 2 {
		t.Fatalf("expected the turn's two replies, got %d: %+v", len(replies), replies)
	}
	first := replies[0]
	for k, want := range map[string]any{
		"tool": codexTool, "role": "assistant", "message_id": "msg_a1", "phase": "commentary",
		"text": "I'll check the command list.", "text_bytes": float64(28), "text_truncated": false,
		"turn_id": replyTurn, "trace_id": replyTrace, "model": "gpt-5.6-luna",
		"evidence_source": "codex_rollout", AttrProjectID: "project-a", "terma.version": "test",
	} {
		if first.Attrs[k] != want {
			t.Errorf("attrs[%q] = %v (%T), want %v", k, first.Attrs[k], first.Attrs[k], want)
		}
	}
	if first.SessionID != replySession || !first.Time.Equal(time.Date(2026, 9, 19, 18, 18, 41, 265_000_000, time.UTC)) {
		t.Errorf("session %q at %v — a reply is stamped with when it was said, in its session", first.SessionID, first.Time)
	}
	if replies[1].Attrs["message_id"] != "msg_a2" || replies[1].Attrs["phase"] != "final_answer" {
		t.Errorf("second reply: %v", replies[1].Attrs)
	}
	// Only what Codex said. The developer's own words already travel — in Codex's export.
	for _, r := range replies {
		if strings.Contains(r.Attrs["text"].(string), "SECRET PROMPT") {
			t.Fatal("the developer's prompt was captured as a reply")
		}
	}
	// A second hook for the same turn — Stop again, or notify beside it — adds nothing.
	if again := stopCodex(t, env, path); len(again) != 0 {
		t.Fatalf("replies spooled twice: %+v", again)
	}
}

// A reply travels under the consent its prompt does, and under no other. `--exclude-prompts`
// is documented as withholding "prompt text or model responses"; this is the second half.
func TestCodexRepliesNeedTheConsentPromptsTravelUnder(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T)
		want  int
	}{
		{"nothing exports Codex here at all", func(*testing.T) {}, 0},
		{"this repository routes Codex without prompts", func(t *testing.T) { routeCodex(t, false) }, 0},
		{"this repository routes Codex with prompts", func(t *testing.T) { routeCodex(t, true) }, 2},
		{"a machine-wide connect with prompts, no routing", func(t *testing.T) { connectCodexMachineWide(t, true) }, 2},
		{"a machine-wide connect without prompts, no routing", func(t *testing.T) { connectCodexMachineWide(t, false) }, 0},
		{"routed with prompts overrides machine-wide exclusion", func(t *testing.T) { routeCodex(t, true); connectCodexMachineWide(t, false) }, 2},
		{"routed exclusion overrides machine-wide prompts", func(t *testing.T) { routeCodex(t, false); connectCodexMachineWide(t, true) }, 0},
		{"both export prompts", func(t *testing.T) { routeCodex(t, true); connectCodexMachineWide(t, true) }, 2},
		{"desktop exporter with no repository route", func(t *testing.T) { connectCodexDesktop(t, true) }, 0},
		{"desktop route excludes prompts", func(t *testing.T) {
			routeCodex(t, false)
			connectCodexDesktop(t, true)
		}, 0},
		{"desktop exporter excludes prompts", func(t *testing.T) {
			routeCodex(t, true)
			connectCodexDesktop(t, false)
		}, 0},
		{"desktop route and exporter allow prompts", func(t *testing.T) {
			routeCodex(t, true)
			connectCodexDesktop(t, true)
		}, 2},
		{"desktop choice is explicitly off", func(t *testing.T) {
			falseValue := false
			if err := shim.SaveRecord(shim.Record{ProjectID: "project-a", Endpoint: "https://otel.terma.ai",
				Signals: []string{"logs"}, IncludePrompts: true, Harnesses: []string{shim.AgentCodex},
				Desktop: &falseValue}); err != nil {
				t.Fatal(err)
			}
			connectCodexDesktop(t, true)
		}, 0},
		{"desktop ignores a dormant repository route", func(t *testing.T) {
			routeCodex(t, true)
			t.Setenv(shim.CodexRoutedEnv, "")
			connectCodexMachineWide(t, false)
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := fundingEnv(t)
			c.setup(t)
			if got := len(stopCodex(t, env, replyRollout(t))); got != c.want {
				t.Fatalf("spooled %d replies, want %d", got, c.want)
			}
			// Without consent the rollout is not so much as opened for replies: no cursor.
			dir, _ := os.ReadDir(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "reply-cursors"))
			if c.want == 0 && len(dir) != 0 {
				t.Fatalf("a reply cursor was written without consent: %v", dir)
			}
		})
	}
}

// "Could not tell" is not consent. A configuration that exists and cannot be read might be
// the one that withholds prompts, so the one place terma reads what was said fails closed —
// even when the *other* configuration, read fine, says yes.
func TestCodexRepliesFailClosedWhenAConsentSourceCannotBeRead(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"a routing record that does not parse, beside a machine-wide connect that allows prompts", func(t *testing.T) {
			connectCodexMachineWide(t, true)
			dir, err := shim.RoutingDir()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, "project-a.json", `{"project_id": "project-a", "include_prompts": tr`)
		}},
		{"a machine-wide config that does not parse, beside a routing record that allows prompts", func(t *testing.T) {
			routeCodex(t, true)
			writeFile(t, os.Getenv("CODEX_HOME"), "config.toml", "[otel\nlog_user_prompt = = true\n")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := fundingEnv(t)
			c.setup(t)
			if got := len(stopCodex(t, env, replyRollout(t))); got != 0 {
				t.Fatalf("spooled %d replies on a consent source that could not be read", got)
			}
			if dir, _ := os.ReadDir(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "reply-cursors")); len(dir) != 0 {
				t.Fatalf("the rollout was opened for replies anyway: %v", dir)
			}
		})
	}
}

// A cursor that does not parse is replaced, and says so — as the funding cursor does. The
// replay is harmless: the ids are Codex's own, so it is the same events again, not new ones.
func TestCodexRepliesReplayFromACorruptCursor(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, true)
	path := replyRollout(t)
	first := stopCodex(t, env, path)
	cursors, _ := os.ReadDir(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "reply-cursors"))
	for _, f := range cursors {
		if strings.HasSuffix(f.Name(), ".json") {
			writeFile(t, filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "reply-cursors"), f.Name(), "{not json")
		}
	}
	var logged strings.Builder
	env.Debug, env.Stderr = true, &logged
	again := stopCodex(t, env, path)
	if len(first) != 2 || len(again) != 2 || again[0].Attrs["message_id"] != first[0].Attrs["message_id"] {
		t.Fatalf("a corrupt cursor should replay the same replies: first %d, again %d", len(first), len(again))
	}
	if !strings.Contains(logged.String(), "invalid reply cursor") {
		t.Fatalf("discarding a cursor should be said: %q", logged.String())
	}
}

// Codex's notify reaches the same capture for a developer with no repository hooks, and
// the two share one locked cursor: a turn's replies are spooled once however many fire.
func TestCodexNotifyAndStopDoNotDoubleReplies(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, true)
	path := replyRollout(t)
	payload, _ := json.Marshal(map[string]any{"type": "agent-turn-complete", "thread-id": replySession, "turn-id": replyTurn, "cwd": env.Cwd,
		"last-assistant-message": "NOT READ FROM HERE"})
	notify := env
	notify.Args = []string{string(payload)}
	if err := CodexNotify(context.Background(), notify); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range spooled(t, env.Spool) {
		if e.Name == EventAssistantMessage {
			total++
			if strings.Contains(e.Attrs["text"].(string), "NOT READ") {
				t.Fatal("the notify payload's last-assistant-message was used; the rollout is the source")
			}
		}
	}
	if total += len(stopCodex(t, env, path)); total != 2 {
		t.Fatalf("notify then Stop spooled %d replies, want the turn's 2 once", total)
	}
}
