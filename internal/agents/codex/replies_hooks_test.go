package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const (
	replySession = "01a0bae3-e62f-72d3-99e7-355f3c7d5fba"
	replyTurn    = "01a0bae4-4a41-7910-9022-15897b3021ae"
	replyTrace   = "ae2a5e6f8e0168ae5ec2efa2c8772b02"
)

// replyRollout writes a one-turn rollout in Codex's shapes and returns its path.
func replyRollout(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+replySession+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + replySession + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39.200Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET PROMPT the developer typed"}]}}`,
		`{"timestamp":"2026-09-19T18:18:41.265Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_a1","phase":"commentary","content":[{"type":"output_text","text":"I'll check the command list."}]}}`,
		`{"timestamp":"2026-09-19T18:19:47.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_a2","phase":"final_answer","content":[{"type":"output_text","text":"Three of them can go."}]}}`,
		`{"timestamp":"2026-09-19T18:19:48.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurn + `"}}`,
	}, "\n")+"\n")
	return path
}

// enableRelay makes this a relay machine: the relay's token exists (claim.Enabled).
func enableRelay(t *testing.T) {
	t.Helper()
	path, err := claim.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// routeCodex makes Codex one of the developer's agents, which setup points at the relay.
func routeCodex(t *testing.T, env *hookrun.Env) {
	t.Helper()
	enableRelay(t)
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) { p.Harnesses = []string{name} }); err != nil {
		t.Fatal(err)
	}
	env.Agents = []string{name}
}

func stopCodex(t *testing.T, env hookrun.Env, path string) []spool.Event {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-5.6-luna"})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	var replies []spool.Event
	for _, e := range hookruntest.Spooled(t, env.Spool) {
		if e.Name == hookrun.EventAssistantMessage {
			replies = append(replies, e)
		}
	}
	return replies
}

// With prompts consented, a turn's end spools Codex's replies in the turn named by its
// trace id, stamped with when they were said.
func TestCodexStopSpoolsWhatCodexSaid(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
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
		"evidence_source": "codex_rollout", hookrun.AttrProjectID: "project-a", "terma.version": "test",
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
	// Only what Codex said: the developer's words already travel in Codex's export.
	for _, r := range replies {
		if strings.Contains(r.Attrs["text"].(string), "SECRET PROMPT") {
			t.Fatal("the developer's prompt was captured as a reply")
		}
	}
	// A second hook for the same turn adds nothing.
	if again := stopCodex(t, env, path); len(again) != 0 {
		t.Fatalf("replies spooled twice: %+v", again)
	}
}

// A reply travels where its prompt would: only for a developer who chose Codex, and only
// while the team's policy collects prompts.
func TestCodexRepliesNeedTheConsentPromptsTravelUnder(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, env *hookrun.Env)
		// withheld is a team policy that does not collect prompts.
		withheld bool
		want     int
	}{
		{"the developer did not choose Codex", func(*testing.T, *hookrun.Env) {}, false, 0},
		{"Codex chosen at setup", routeCodex, false, 2},
		{"Codex chosen, the team withholds prompts", routeCodex, true, 0},
		{"Codex chosen, whatever its own exporter's syntax", func(t *testing.T, env *hookrun.Env) {
			routeCodex(t, env)
			hookruntest.WriteFile(t, os.Getenv("CODEX_HOME"), "config.toml", "[otel\ninvalid\n")
		}, false, 2},
		{"Codex chosen, but no relay on this machine", func(t *testing.T, env *hookrun.Env) {
			routeCodex(t, env)
			path, _ := claim.TokenPath()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := fundingEnv(t)
			env.Policy.IncludePrompts = !c.withheld
			c.setup(t, &env)
			// The hook knows no policy; delivery withholds replies the team's prompts-off does.
			if got := len(delivered(env.Policy, stopCodex(t, env, replyRollout(t)))); got != c.want {
				t.Fatalf("delivered %d replies, want %d", got, c.want)
			}
			// Where nothing routes Codex the rollout is not opened for replies: no cursor.
			dir, _ := os.ReadDir(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "reply-cursors"))
			if c.want == 0 && !c.withheld && len(dir) != 0 {
				t.Fatalf("a reply cursor was written without consent: %v", dir)
			}
		})
	}
}

// A cursor that does not parse is replaced and reported; the replay repeats Codex's own ids.
func TestCodexRepliesReplayFromACorruptCursor(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := replyRollout(t)
	first := stopCodex(t, env, path)
	cursors, _ := os.ReadDir(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "reply-cursors"))
	for _, f := range cursors {
		if strings.HasSuffix(f.Name(), ".json") {
			hookruntest.WriteFile(t, filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "reply-cursors"), f.Name(), "{not json")
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

// notify shares Stop's locked cursor, so a turn's replies are spooled once.
func TestCodexNotifyAndStopDoNotDoubleReplies(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := replyRollout(t)
	payload, _ := json.Marshal(map[string]any{"type": "agent-turn-complete", "thread-id": replySession, "turn-id": replyTurn, "cwd": env.Cwd,
		"last-assistant-message": "NOT READ FROM HERE"})
	notify := env
	notify.Args = []string{string(payload)}
	if err := notifyHook(context.Background(), notify); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range hookruntest.Spooled(t, env.Spool) {
		if e.Name == hookrun.EventAssistantMessage {
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

// Replies need the relay and Codex among the developer's agents, or global mode.
func TestRepliesConsented(t *testing.T) {
	for c, want := range map[*hookrun.Consent]bool{
		{Agents: []string{name}, Relay: true}:     true,
		{Relay: true, Global: true}:               true,
		{Agents: []string{"claude"}, Relay: true}: false,
		{Agents: []string{name}}:                  false,
		{Global: true}:                            false,
	} {
		if got := repliesConsented(*c); got != want {
			t.Errorf("repliesConsented(%+v) = %v, want %v", *c, got, want)
		}
	}
}
