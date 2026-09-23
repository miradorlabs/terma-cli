package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The real interactive harness consumes a controlled response in a fresh home.
// This verifies renderer preservation without needing a subscription setup token;
// authentic provider/API contracts remain separate credential-gated scenarios.
func TestClaudeIsolatedRenderer(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		const key = "synthetic-renderer-key"
		t.Setenv("ANTHROPIC_API_KEY", key)
		sb := New(t, Isolated, WithClaude(b))
		// Interactive Claude requires approval even for a loopback dummy key.
		// Its scratch config stores the final 20 characters, as observed in the
		// versioned binary. This fixture contains no real credential.
		statePath := filepath.Join(sb.ClaudeConfig, ".claude.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		var state map[string]any
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		state["customApiKeyResponses"] = map[string]any{"approved": []string{key[len(key)-20:]}, "rejected": []string{}}
		data, _ = json.Marshal(state)
		sb.writeAbs(statePath, string(data))
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
				return
			}
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []struct{ name, data string }{
				{"message_start", `{"type":"message_start","message":{"id":"msg_live_renderer","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":0}}}`},
				{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"TERMA_OK"}}`},
				{"content_block_stop", `{"type":"content_block_stop","index":0}`},
				{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`},
				{"message_stop", `{"type":"message_stop"}`},
			} {
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, event.data)
			}
		}))
		defer server.Close()
		sb.ClaudeBaseURL = server.URL
		run := sb.ClaudeInteractive(RouteAPIKey, "Reply with exactly TERMA_OK and nothing else.", termaOK, "--tools", "")
		if calls.Load() == 0 || !strings.Contains(run.Text, sb.RendererMarker) || !markRE.MatchString(run.Text) {
			t.Fatalf("isolated renderer was not preserved: calls=%d\n%s", calls.Load(), tail(run.Text, 2000))
		}
		checkClaudeAccount(t, sb, run.SessionID, false)
		Note("claude/isolated-renderer", "real interactive harness; synthetic loopback response; original renderer and Terma mark preserved")
	})
}

func checkClaudeAccount(t *testing.T, sb *Sandbox, sid string, requireAccount bool) {
	t.Helper()
	events := sb.Delivered("terma.session.account", sid, 30*time.Second)
	if len(events) == 0 {
		t.Errorf("no Claude account evidence delivered for %s", sid)
		return
	}
	a := events[len(events)-1].Attrs
	if a["evidence_source"] != "claude_account" || a["project_id"] != sb.ProjectID {
		t.Errorf("account source/project: %v", a)
	}
	if requireAccount && a["evidence_status"] != "present" {
		t.Errorf("stored login not observed: %v", a)
	}
	if requireAccount {
		for _, key := range []string{"account_id", "billing_type", "organization_type", "seat_tier"} {
			if a[key] == "" {
				t.Errorf("account snapshot lacks %s", key)
			}
		}
	}
	for _, key := range []string{"api_key_present", "auth_token_present"} {
		if a[key] != "true" && a[key] != "false" {
			t.Errorf("credential hint %s is not boolean", key)
		}
	}
	for _, key := range []string{"accessToken", "refreshToken", "emailAddress", "apiKeyHelper", "error_details", "last_assistant_message"} {
		if _, ok := a[key]; ok {
			t.Errorf("private field %s in account evidence", key)
		}
	}
}

func TestClaudeMultipleTurns(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		mode, route := claudeMode(t)
		sb := New(t, mode, WithClaude(b))
		seen := map[string]bool{}
		run := sb.ClaudeInteractiveTurns(route, []string{
			"Reply with exactly TERMA_FIRST_OK and nothing else.",
			"Reply with exactly TERMA_SECOND_OK and nothing else.",
		}, []*regexp.Regexp{regexp.MustCompile(`⏺\s*TERMA_FIRST_OK`), regexp.MustCompile(`⏺\s*TERMA_SECOND_OK`)}, func(turn int, sid string) {
			deadline := time.Now().Add(20 * time.Second)
			for {
				quota := sb.Delivered("terma.session.quota", sid, time.Second)
				reqs := sb.APIRequests(sid, time.Second)
				var latest *LogRecord
				for i := range quota {
					if latest == nil || quota[i].Time.After(latest.Time) {
						latest = &quota[i]
					}
				}
				if latest != nil && latest.Attrs["prompt_id"] != "" && !seen[latest.Attrs["prompt_id"]] && latest.Attrs["session_cost_usd"] != "0" {
					joined := false
					for _, r := range reqs {
						if r.Attrs["prompt.id"] == latest.Attrs["prompt_id"] {
							joined = true
						}
					}
					if joined {
						seen[latest.Attrs["prompt_id"]] = true
						return
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("turn %d: no new prompt's quota delivered while session remains open", turn+1)
				}
				time.Sleep(200 * time.Millisecond)
			}
		}, "--tools", "")
		if len(seen) != 2 {
			t.Errorf("want two prompt snapshots, got %d", len(seen))
		}
		checkClaudeAccount(t, sb, run.SessionID, mode == RealLogin)
		AddSpend(SpendOf(sb.APIRequests(run.SessionID, time.Second)))
	})
}

// A real Claude process against a synthetic, loopback API error exercises the
// provider's StopFailure wiring without exhausting a real account's allowance.
func TestClaudeStopFailureDelivery(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-live-error-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.connectClaude()
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"synthetic error for hook verification"}}`))
		}))
		defer server.Close()
		sid := "00000000-0000-4000-8000-000000000123"
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		args := append([]string{"-p", "Reply TERMA_OK", "--output-format", "json", "--max-turns", "1"}, sb.claudeArgs(sid, "--tools", "")...)
		cmd := exec.CommandContext(ctx, b.Path, args...)
		cmd.Dir = sb.Repo
		sb.ClaudeBaseURL = server.URL
		cmd.Env = sb.claudeEnv(RouteAPIKey)
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil || calls.Load() == 0 {
			t.Fatalf("synthetic API was not exercised: %v %s", err, tail(string(out), 1500))
		}
		limits := sb.Delivered("terma.session.limit", sid, 30*time.Second)
		if len(limits) == 0 {
			t.Fatalf("StopFailure did not deliver a limit event: %s", tail(string(out), 1500))
		}
		a := limits[len(limits)-1].Attrs
		if a["error_type"] != "unknown" || a["evidence_source"] != "claude_stop_failure" {
			t.Errorf("failure event: %v", a)
		}
		for k, v := range a {
			if strings.Contains(v, "synthetic error") || k == "error_details" || k == "last_assistant_message" {
				t.Errorf("failure detail escaped via %s", k)
			}
		}
		checkClaudeAccount(t, sb, sid, false)
		Note("claude/StopFailure", "real harness; synthetic loopback 400 is classified as unknown")
	})
}
