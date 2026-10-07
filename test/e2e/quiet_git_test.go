package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedClaudeProvider answers Claude Code's first calls with one tool use each, in
// order, then a text reply.
func scriptedClaudeProvider(calls *atomic.Int32, tools []map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		call := int(calls.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(name string, payload any) {
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_scripted_%d", call), "type": "message", "role": "assistant", "model": "claude-haiku-4-5", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 12, "output_tokens": 0}}})
		stop := "end_turn"
		if call <= len(tools) {
			stop = "tool_use"
			tool := tools[call-1]
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_scripted_%d", call), "name": tool["name"], "input": map[string]any{}}})
			input, _ := json.Marshal(tool["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		} else {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "TERMA_OK"}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}})
		emit("message_stop", map[string]any{"type": "message_stop"})
	})
}

// TestClaudeQuietCommitAndPush: a session writes a file, then commits and pushes it with
// git's output silenced, so nothing in what Claude Code exports names the commit or the
// push. The repository's hooks still record both: terma.commit with the full sha and the
// working tree, and terma.push with the commit the remote now has. The relay names the
// same working tree on Claude Code's own records of the Bash call.
func TestClaudeQuietCommitAndPush(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		remote := filepath.Join(sb.Dir, "remote.git")
		sb.gitIn(sb.Dir, "init", "-q", "--bare", remote)
		sb.git("remote", "add", "up", remote)
		file := filepath.Join(sb.Repo, "quiet.txt")
		var calls atomic.Int32
		provider := httptest.NewServer(scriptedClaudeProvider(&calls, []map[string]any{
			{"name": "Write", "input": map[string]any{"file_path": file, "content": "quiet\n"}},
			{"name": "Bash", "input": map[string]any{"command": "git add quiet.txt && git commit -q -m 'quiet commit' && git push -q up HEAD:refs/heads/main 2>/dev/null"}},
		}))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		_, sid := sb.ClaudeHeadless(RouteAPIKey, "Write the file, then commit and push it.", "--max-turns", "4",
			"--tools", "Write,Bash", "--allowedTools", "Write,Bash")

		pushed := strings.TrimSpace(sb.gitIn(sb.Dir, "--git-dir", remote, "rev-parse", "main"))
		tree, _ := filepath.EvalSymlinks(sb.Repo)
		commits := sb.Delivered("terma.commit", sid, 30*time.Second)
		if len(commits) != 1 {
			t.Fatalf("terma.commit for %s: %d; spool: %+v", sid, len(commits), sb.Spool())
		}
		c := commits[0].Attrs
		if c["vcs.ref.head.revision"] != pushed || c["terma.repository.root"] != tree || !strings.Contains(c["terma.commit.file.stats"], "quiet.txt") {
			t.Errorf("terma.commit = %v; want sha %s, root %s, quiet.txt", c, pushed, tree)
		}
		pushes := sb.Delivered("terma.push", sid, 30*time.Second)
		if len(pushes) != 1 {
			t.Fatalf("terma.push for %s: %d; spool: %+v", sid, len(pushes), sb.Spool())
		}
		p := pushes[0].Attrs
		if !slices.Contains(listAttr(p["terma.push.commit.shas"]), pushed) || p["terma.push.remote.name"] != "up" ||
			p["terma.repository.root"] != tree || !strings.Contains(p["terma.push.refs"], "refs/heads/main") {
			t.Errorf("terma.push = %v; want %s on up's refs/heads/main from %s", p, pushed, tree)
		}
		bash := sb.Receiver.WaitLogs(30*time.Second, func(l LogRecord) bool {
			return l.Resource["service.name"] != "terma-cli" && l.Attrs["session.id"] == sid && l.Attrs["tool_name"] == "Bash"
		})
		for _, l := range bash {
			if l.Resource["terma.repository.root"] != tree {
				t.Errorf("Claude Code's %s for Bash: terma.repository.root = %q, want %q", l.Attrs["event.name"], l.Resource["terma.repository.root"], tree)
			}
		}
		if len(bash) == 0 {
			t.Error("no Claude Code record of the Bash call reached the receiver")
		}
	})
}
