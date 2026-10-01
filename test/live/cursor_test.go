package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This exercises the actual CLI hook loader, two turns in one conversation, file
// attribution and detached OTLP delivery. It needs an explicitly supplied Cursor
// CLI key; it never reads the developer's browser cookies or IDE credentials.
func TestCursorCLITurnObservations(t *testing.T) {
	cursorSession(t, false, false)
}

func TestCursorInteractiveTurnObservations(t *testing.T) {
	cursorSession(t, false, true)
}

// This validates conversation-level billing joins even when a headless Cursor
// release emits only lifecycle hooks. Turn-hook parity is tested separately.
func TestCursorBillingSession(t *testing.T) {
	cursorSession(t, true, false)
}

func cursorSession(t *testing.T, billingOnly, interactive bool) {
	t.Helper()
	track(t)
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
	}
	key := os.Getenv("CURSOR_API_KEY")
	if key == "" {
		t.Skip("needs CURSOR_API_KEY (CLI key, not a team admin key)")
	}
	binary := os.Getenv("TERMA_LIVE_CURSOR_BINARY")
	if binary == "" {
		binary = "cursor-agent"
	}
	binary, err := exec.LookPath(binary)
	if err != nil {
		t.Skip("Cursor CLI is not installed")
	}
	startedAt := time.Now().UTC()
	completedTurns := 0
	validationKind := "turn_hooks"
	if billingOnly {
		validationKind = "session_billing"
	} else if interactive {
		validationKind = "interactive_turn_hooks"
	}
	sb := New(t, Isolated)
	// Opt-in private artifact for the standalone billing-import validation.
	// Export only delivered observation fields; never raw hook payloads.
	t.Cleanup(func() {
		path := os.Getenv("TERMA_LIVE_CURSOR_CAPTURE")
		if path == "" {
			return
		}
		observations := []map[string]string{}
		for _, record := range sb.Receiver.Logs() {
			if record.Attrs["event.name"] != "terma.session.observation" || record.Attrs["tool"] != "cursor" {
				continue
			}
			a := map[string]string{}
			for _, key := range []string{"session.id", "turn_id", "account_email", "hook_event", "model", "model_id", "usage_status", "reported_input_tokens", "reported_output_tokens", "reported_cache_read_tokens", "reported_cache_write_tokens", "observation_id", "observation_sequence", "source_stream"} {
				if value, ok := record.Attrs[key]; ok {
					a[key] = value
				}
			}
			observations = append(observations, a)
		}
		data, err := json.MarshalIndent(map[string]any{"version": 1, "validation_kind": validationKind, "completed_turns": completedTurns, "started_at": startedAt, "ended_at": time.Now().UTC(), "cli_version": Version(binary), "test_passed": !t.Failed(), "observations": observations}, "", "  ")
		if err == nil {
			err = os.MkdirAll(filepath.Dir(path), 0o700)
		}
		if err == nil {
			err = os.WriteFile(path, append(data, '\n'), 0o600)
		}
		if err != nil {
			t.Errorf("write Cursor validation capture: %v", err)
		}
	})
	sb.terma(sb.Repo, "install", "--harness", "none", "--no-browser", "--no-doctor", "--project", sb.ProjectID, "--adapters", "cursor", "--yes")
	// No local Cursor exporter exists to connect. Give only the sandbox spool
	// the loopback receiver's dummy project key.
	keys, _ := json.Marshal(map[string]any{"keys": map[string]string{sb.ProjectID: liveKey}})
	if err = os.WriteFile(filepath.Join(sb.TermaConfig, "keys.json"), keys, 0o600); err != nil {
		t.Fatal(err)
	}
	invocation := 0
	var terminal *Terminal
	t.Cleanup(func() {
		if terminal != nil {
			_ = terminal.Close("/quit", 5*time.Second)
			if capture := os.Getenv("TERMA_LIVE_CURSOR_CAPTURE"); capture != "" {
				path := filepath.Join(filepath.Dir(capture), "terminal.txt")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
					_ = os.WriteFile(path, []byte(strings.ReplaceAll(terminal.Text(), key, "[REDACTED]")), 0o600)
				}
			}
		}
	})
	run := func(prompt, resume string) string {
		t.Helper()
		if interactive {
			before := len(sb.HookPayloads("cursor-stop"))
			if terminal == nil {
				terminal, err = Start(sb.Repo, append(sb.termaEnv(), "CURSOR_API_KEY="+key, "NO_OPEN_BROWSER=1"), 40, 140, binary,
					"--trust", "--force", "--workspace", sb.Repo, prompt)
			} else {
				err = terminal.Type(prompt)
			}
			if err != nil {
				t.Fatalf("start/send Cursor interactive turn: %v", err)
			}
			deadline := time.Now().Add(scenarioTimeout)
			for time.Now().Before(deadline) {
				payloads := sb.HookPayloads("cursor-stop")
				if len(payloads) > before {
					p := payloads[len(payloads)-1]
					id, _ := p["conversation_id"].(string)
					if id == "" || p["status"] != "completed" {
						t.Fatal("interactive Stop lacks conversation identity or completed status")
					}
					completedTurns++
					return id
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatal("Cursor interactive turn did not deliver Stop; see private terminal capture")
		}
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		args := []string{"--print", "--trust", "--force", "--output-format", "json", "--workspace", sb.Repo}
		if resume != "" {
			args = append(args, "--resume", resume)
		}
		args = append(args, prompt)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = sb.Repo
		cmd.Env = append(sb.termaEnv(), "CURSOR_API_KEY="+key, "NO_OPEN_BROWSER=1")
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		invocation++
		if capture := os.Getenv("TERMA_LIVE_CURSOR_CAPTURE"); capture != "" {
			// Only this test's synthetic responses, in the private capture directory.
			path := filepath.Join(filepath.Dir(capture), fmt.Sprintf("cli-response-%d.json", invocation))
			if e := os.MkdirAll(filepath.Dir(path), 0o700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(path, []byte(strings.ReplaceAll(stdout.String(), key, "[REDACTED]")), 0o600); e != nil {
				t.Fatal(e)
			}
		}
		if err != nil {
			t.Fatalf("Cursor CLI failed: %v (output omitted to protect credentials)", err)
		}
		var response struct {
			IsError   bool   `json:"is_error"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(stdout.Bytes(), &response) != nil || response.IsError || response.SessionID == "" {
			t.Fatal("Cursor CLI returned an error or invalid JSON; see private validation artifact")
		}
		completedTurns++
		return response.SessionID
	}
	conversation := run("Create cursor-live.txt containing exactly first turn. Use the file editing tool. Do not commit or use subagents.", "")
	checkFile := func(want string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(sb.Repo, "cursor-live.txt"))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Fatalf("Cursor did not write expected file contents: %v", err)
		}
	}
	checkFile("first turn")
	if resumed := run("Change cursor-live.txt to contain exactly second turn. Use the file editing tool. Do not commit or use subagents.", conversation); resumed != conversation {
		t.Fatal("Cursor resume changed conversation ID")
	}
	checkFile("second turn")
	// Force a final pass too, to make failures distinguishable from flush timing.
	sb.terma(sb.Repo, "spool", "flush", "--quiet")
	delivered := sb.Delivered("terma.session.observation", conversation, 10*time.Second)
	if !billingOnly {
		// The recording shim writes the payload before Terma processes it. A
		// first received observation does not mean the second Stop's detached
		// flush has finished; wait for both generations before asserting.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			stops := map[string]bool{}
			for _, record := range delivered {
				if record.Attrs["hook_event"] == "stop" && record.Attrs["turn_id"] != "" {
					stops[record.Attrs["turn_id"]] = true
				}
			}
			if len(stops) >= 2 {
				break
			}
			time.Sleep(100 * time.Millisecond)
			delivered = sb.Delivered("terma.session.observation", conversation, time.Second)
		}
	}
	if len(delivered) == 0 {
		t.Fatal("no Cursor observations delivered")
	}
	if billingOnly {
		starts, ends := 0, 0
		for _, record := range delivered {
			if record.Attrs["tool"] != "cursor" || record.Attrs["account_email"] == "" {
				t.Fatal("missing Cursor account identity in conversation observations")
			}
			switch record.Attrs["hook_event"] {
			case "sessionStart":
				starts++
			case "sessionEnd":
				ends++
			}
		}
		// Conversation-level attribution needs an observed identity, not a new
		// lifecycle pair per resume. Both CLI results and file edits are checked
		// above; this does not establish per-turn hook coverage.
		if starts < 1 || ends < 1 {
			t.Fatalf("missing conversation lifecycle delivery; starts=%d ends=%d", starts, ends)
		}
		return
	}
	turns := map[string]bool{}
	sequences := map[string]map[uint64]string{}
	for _, record := range delivered {
		a := record.Attrs
		if a["tool"] != "cursor" || a["ordering"] != "local_receipt" || a["funding_status"] != "unavailable" || a["quota_status"] != "unavailable" {
			t.Fatalf("incorrect observation contract: %v", a)
		}
		seq, err := strconv.ParseUint(a["observation_sequence"], 10, 64)
		if err != nil || seq == 0 || a["observation_id"] == "" || a["source_stream"] == "" {
			t.Fatal("missing durable observation identity")
		}
		stream := a["source_stream"]
		if sequences[stream] == nil {
			sequences[stream] = map[uint64]string{}
		}
		if old := sequences[stream][seq]; old != "" && old != a["observation_id"] {
			t.Fatal("sequence position reused")
		}
		sequences[stream][seq] = a["observation_id"]
		if a["hook_event"] == "stop" {
			if a["turn_id"] == "" {
				t.Fatal("Cursor stop omitted generation_id")
			}
			turns[a["turn_id"]] = true
			if a["usage_status"] == "" {
				t.Fatal("missing usage availability")
			}
		}
	}
	if len(turns) < 2 {
		t.Fatalf("expected two stop generations; got %d", len(turns))
	}
	for stream, positions := range sequences {
		for i := uint64(1); i <= uint64(len(positions)); i++ {
			if positions[i] == "" {
				t.Errorf("gap at %s/%d", stream, i)
			}
		}
	}
	// Compare raw optional values with delivered snapshots; do not infer absent
	// usage or add response and stop snapshots together.
	for _, p := range sb.HookPayloads("cursor-stop") {
		if p["conversation_id"] != conversation {
			continue
		}
		generation, _ := p["generation_id"].(string)
		found := false
		for _, record := range delivered {
			a := record.Attrs
			if a["hook_event"] != "stop" || a["turn_id"] != generation {
				continue
			}
			found = true
			for _, field := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"} {
				value, present := p[field]
				if !present || value == nil {
					if _, exists := a["reported_"+field]; exists {
						t.Errorf("missing %s became present", field)
					}
					continue
				}
				if a["reported_"+field] != fmt.Sprint(value) {
					t.Errorf("%s differs from hook payload", field)
				}
			}
		}
		if !found {
			t.Errorf("stop generation %s not delivered", generation)
		}
	}
	if len(sb.HookPayloads("cursor-file-edit")) == 0 {
		t.Fatal("Cursor did not supply file attribution")
	}
	msg := sb.Commit("Cursor live test")
	if !strings.Contains(msg, "Agent-Session-Id: "+conversation) || !strings.Contains(msg, "Agent-Tool: cursor") {
		t.Fatal("Cursor commit attribution missing")
	}
}

// Synthetic payloads through the installed shims and real binary. This verifies
// delivery without provider credentials; it does not verify Cursor itself.
func TestCursorHookDelivery(t *testing.T) {
	track(t)
	sb := New(t, Isolated)
	sb.terma(sb.Repo, "install", "--harness", "none", "--no-browser", "--no-doctor", "--project", sb.ProjectID, "--adapters", "cursor", "--yes")
	keys, _ := json.Marshal(map[string]any{"keys": map[string]string{sb.ProjectID: liveKey}})
	if err := os.WriteFile(filepath.Join(sb.TermaConfig, "keys.json"), keys, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(sb.Repo, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hooks struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err = json.Unmarshal(raw, &hooks); err != nil {
		t.Fatal(err)
	}
	run := func(hook, turn string, disabled bool) {
		t.Helper()
		entries := hooks.Hooks[hook]
		if len(entries) != 1 {
			t.Fatalf("missing installed %s", hook)
		}
		payload, _ := json.Marshal(map[string]any{"conversation_id": "synthetic-cursor", "generation_id": turn, "workspace_roots": []string{sb.Repo}, "hook_event_name": hook, "model": "synthetic-model", "status": "completed", "input_tokens": 10, "output_tokens": 2, "cache_read_tokens": 0, "cache_write_tokens": 0, "prompt": "private-prompt", "text": "private-response",
			// postToolUse fields; the other hooks ignore them, and the output must never leave the machine.
			"tool_name": "Shell", "tool_use_id": "call-" + turn, "duration": 5432, "tool_input": map[string]any{"command": "private-command"}, "tool_output": "private-output"})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", entries[0].Command)
		cmd.Dir = sb.Repo
		cmd.Env = sb.termaEnv()
		if disabled {
			cmd.Env = append(cmd.Env, "TERMA_HOOKS=0")
		}
		cmd.Stdin = strings.NewReader(string(payload))
		out, err := cmd.CombinedOutput()
		if err != nil || len(out) > 0 {
			t.Fatalf("hook changed terminal output: err=%v output=%q", err, out)
		}
	}
	for _, turn := range []string{"turn-a", "turn-b"} {
		run("beforeSubmitPrompt", turn, false)
		run("postToolUse", turn, false)
		run("afterAgentResponse", turn, false)
		run("stop", turn, false)
	}
	run("stop", "disabled-turn", true)
	sb.terma(sb.Repo, "spool", "flush", "--quiet")
	calls := sb.Delivered("terma.tool.call", "synthetic-cursor", 10*time.Second)
	if len(calls) != 2 {
		t.Fatalf("delivered %d tool calls, want 2", len(calls))
	}
	for _, record := range calls {
		a := record.Attrs
		if a["tool_name"] != "Shell" || !strings.HasPrefix(a["tool_call_id"], "call-turn-") || a["duration_ms"] != "5432" || a["status"] != "completed" || a["hook_event"] != "postToolUse" {
			t.Fatalf("tool call attributes: %+v", a)
		}
		for _, v := range a {
			if strings.Contains(v, "private-") {
				t.Fatal("tool input or output escaped")
			}
		}
	}
	logs := sb.Delivered("terma.session.observation", "synthetic-cursor", 10*time.Second)
	ids := map[string]bool{}
	sequences := map[string]bool{}
	for _, record := range logs {
		a := record.Attrs
		ids[a["observation_id"]] = true
		sequences[a["observation_sequence"]] = true
		if a["turn_id"] == "disabled-turn" {
			t.Fatal("kill switch failed")
		}
		if a["hook_event"] == "stop" && a["reported_cache_write_tokens"] != "0" {
			t.Fatal("explicit zero did not survive OTLP")
		}
		for _, v := range a {
			if strings.Contains(v, "private-") {
				t.Fatal("private content escaped")
			}
		}
	}
	if len(ids) != 6 {
		t.Fatalf("delivered %d unique observations, want 6", len(ids))
	}
	for i := 1; i <= 6; i++ {
		if !sequences[strconv.Itoa(i)] {
			t.Fatalf("missing sequence %d", i)
		}
	}
}
