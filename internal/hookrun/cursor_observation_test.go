package hookrun

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

func cursorPayload(t *testing.T, env Env, turn string, fields map[string]any) string {
	t.Helper()
	p := map[string]any{"conversation_id": "cursor-conversation", "generation_id": turn, "workspace_roots": []string{env.Cwd}, "model": "auto", "model_id": "selected-model", "user_email": "dev@example.test", "cursor_version": "2026.09.10-fd3934a"}
	maps.Copy(p, fields)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func cursorRun(t *testing.T, env Env, hook, turn string, fields map[string]any) {
	t.Helper()
	env.Stdin = strings.NewReader(cursorPayload(t, env, turn, fields))
	if err := cursorObserve(context.Background(), env, hook); err != nil {
		t.Fatal(err)
	}
}
func cursorObservations(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var out []spool.Event
	for _, e := range spooledQuota(t, sp) {
		if e.Name == EventSessionObservation {
			out = append(out, e)
		}
	}
	return out
}
func cursorStatePath(env Env) string {
	return filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "cursor-observations", evidenceID("cursor-conversation\x00"+env.Cwd)+".json")
}

func TestCursorOrderedTurnSnapshots(t *testing.T) {
	env := fundingEnv(t)
	private := map[string]any{"prompt": "secret-prompt", "text": "secret-response", "transcript_path": "/secret/path", "api_key": "secret-key", "status": "completed", "loop_count": 0, "input_tokens": 100, "output_tokens": 12, "cache_read_tokens": 80, "cache_write_tokens": 0}
	for _, turn := range []string{"turn-a", "turn-b"} {
		cursorRun(t, env, "beforeSubmitPrompt", turn, private)
		cursorRun(t, env, "afterAgentResponse", turn, private)
		cursorRun(t, env, "stop", turn, private)
		cursorRun(t, env, "stop", turn, private) // Adjacent duplicate only.
	}
	// Account/usage disappearance must replace the prior observation, not carry it forward.
	cursorRun(t, env, "stop", "turn-b", map[string]any{"user_email": nil, "status": "aborted"})
	evs := cursorObservations(t, env.Spool)
	if len(evs) != 7 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	ids := map[any]bool{}
	stream := evs[0].Attrs["source_stream"]
	for i, e := range evs {
		a := e.Attrs
		if a["observation_sequence"] != float64(i+1) || a["source_stream"] != stream || a[AttrProjectID] != "project-a" {
			t.Fatalf("bad sequence/routing: %+v", e)
		}
		if ids[a["observation_id"]] {
			t.Fatal("reused observation ID")
		}
		ids[a["observation_id"]] = true
		if a["funding_status"] != "unavailable" || a["quota_status"] != "unavailable" {
			t.Fatal("invented funding evidence")
		}
		if _, ok := a["source_time"]; ok {
			t.Fatal("invented provider timestamp")
		}
	}
	if evs[1].Attrs["reported_cache_write_tokens"] != float64(0) || evs[1].Attrs["usage_status"] != "available" {
		t.Fatal(evs[1])
	}
	if evs[3].Attrs["turn_id"] != "turn-b" {
		t.Fatal("identical values on another turn were suppressed")
	}
	last := evs[6].Attrs
	if last["usage_status"] != "unavailable" || last["account_status"] != "unavailable" {
		t.Fatal(last)
	}
	if _, ok := last["reported_input_tokens"]; ok {
		t.Fatal("missing became zero or stale")
	}
	b, _ := json.Marshal(evs)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("private content escaped: %s", b)
	}
}

func TestCursorInvalidOptionalTokensAndContext(t *testing.T) {
	env := fundingEnv(t)
	cursorRun(t, env, "stop", "turn", map[string]any{"input_tokens": -1, "output_tokens": "12", "cache_read_tokens": 1.5, "cache_write_tokens": 9007199254740992, "status": "secret-error"})
	cursorRun(t, env, "stop", "turn", map[string]any{"input_tokens": 0, "output_tokens": nil, "status": "error", "loop_count": 6})
	cursorRun(t, env, "preCompact", "turn", map[string]any{"context_usage_percent": 85.5, "context_tokens": 120000, "context_window_size": 128000, "trigger": "auto", "messages": []string{"secret-content"}})
	evs := cursorObservations(t, env.Spool)
	if len(evs) != 3 {
		t.Fatal(evs)
	}
	if evs[0].Attrs["usage_status"] != "invalid" || evs[0].Attrs["status"] != "unknown" {
		t.Fatal(evs[0])
	}
	for _, k := range []string{"input", "output", "cache_read", "cache_write"} {
		if _, ok := evs[0].Attrs["reported_"+k+"_tokens"]; ok {
			t.Fatal("invalid count accepted")
		}
	}
	if evs[1].Attrs["usage_status"] != "partial" || evs[1].Attrs["reported_input_tokens"] != float64(0) || evs[1].Attrs["loop_count"] != float64(6) {
		t.Fatal(evs[1])
	}
	if evs[2].Attrs["context_usage_percent"] != 85.5 || evs[2].Attrs["quota_status"] != "unavailable" {
		t.Fatal(evs[2])
	}
}

func TestCursorConcurrentObservations(t *testing.T) {
	env := fundingEnv(t)
	var wg sync.WaitGroup
	for i := range 12 {
		payload := cursorPayload(t, env, fmt.Sprintf("turn-%d", i), map[string]any{"output_tokens": i})
		wg.Go(func() {
			e := env
			e.Stdin = strings.NewReader(payload)
			_ = CursorStop(context.Background(), e)
		})
	}
	wg.Wait()
	evs := cursorObservations(t, env.Spool)
	if len(evs) != 12 {
		t.Fatalf("lost concurrent hooks: %d", len(evs))
	}
	turns := map[any]bool{}
	for i, e := range evs {
		if e.Attrs["observation_sequence"] != float64(i+1) {
			t.Fatal(e)
		}
		turns[e.Attrs["turn_id"]] = true
	}
	if len(turns) != 12 {
		t.Fatal("lost turn identity")
	}
}

func TestCursorPendingReplayKeepsIdentity(t *testing.T) {
	env := fundingEnv(t)
	cursorRun(t, env, "stop", "turn-a", nil)
	original := cursorObservations(t, env.Spool)[0]
	path := cursorStatePath(env)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state observationState
	if err = json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after append but before acknowledging the checkpoint.
	state.Pending = &original
	b, _ = json.Marshal(state)
	if err = os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cursorRun(t, env, "stop", "turn-b", nil)
	evs := cursorObservations(t, env.Spool)
	if len(evs) != 2 || evs[0].Attrs["observation_id"] != original.Attrs["observation_id"] || evs[1].Attrs["observation_sequence"] != float64(2) {
		t.Fatal(evs)
	}
}

func TestCursorFailedAppendIsRecoveredBeforeNextTurn(t *testing.T) {
	env := fundingEnv(t)
	badDir := t.TempDir()
	badSpool, err := spool.Open(badDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(badDir, "events.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	failed := env
	failed.Spool = badSpool
	cursorRun(t, failed, "stop", "turn-a", nil)
	b, err := os.ReadFile(cursorStatePath(env))
	if err != nil {
		t.Fatal(err)
	}
	var state observationState
	_ = json.Unmarshal(b, &state)
	if state.Pending == nil {
		t.Fatal("failed append lost its evidence")
	}
	cursorRun(t, env, "stop", "turn-b", nil)
	evs := cursorObservations(t, env.Spool)
	if len(evs) != 2 || evs[0].Attrs["turn_id"] != "turn-a" || evs[1].Attrs["turn_id"] != "turn-b" {
		t.Fatal(evs)
	}
}

func TestCursorCorruptCheckpointStartsNewStream(t *testing.T) {
	env := fundingEnv(t)
	cursorRun(t, env, "stop", "turn-a", nil)
	first := cursorObservations(t, env.Spool)[0]
	if err := os.WriteFile(cursorStatePath(env), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cursorRun(t, env, "stop", "turn-b", nil)
	second := cursorObservations(t, env.Spool)[0]
	if second.Attrs["capture_gap"] != "invalid_checkpoint" || second.Attrs["source_stream"] == first.Attrs["source_stream"] {
		t.Fatal(second)
	}
}

func TestCursorPromptRefreshesAttributionAndMissingGenerationIsNotInherited(t *testing.T) {
	env := fundingEnv(t)
	cursorRun(t, env, "beforeSubmitPrompt", "turn-a", nil)
	env.Now = env.Now.Add(ActiveTTL + time.Minute)
	cursorRun(t, env, "beforeSubmitPrompt", "turn-b", nil)
	r, err := env.repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := r.store.Active(env.Now, ActiveTTL); !ok || s.ID != "cursor-conversation" {
		t.Fatal("prompt did not refresh active session")
	}
	cursorRun(t, env, "stop", "", nil)
	evs := cursorObservations(t, env.Spool)
	if _, ok := evs[len(evs)-1].Attrs["turn_id"]; ok {
		t.Fatal("missing turn inherited another turn ID")
	}
}

func TestCursorInputBound(t *testing.T) {
	for _, p := range []string{`{"conversation_id":"valid"}` + strings.Repeat(" ", 4<<20), `{"conversation_id":"../../bad"}`, `{"conversation_id":""}`} {
		if _, err := readCursorInput(strings.NewReader(p)); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
}
