package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// Every handler refuses a session id unsafe as a file name or query value, whichever key
// carries it, without failing the hook.
func TestNoHandlerSpoolsAnUnsafeSessionID(t *testing.T) {
	root, sp := hookruntest.Project(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	unsafe := []string{"../../etc/passwd", "two\nlines", strings.Repeat("a", 4096)}
	// Control: with a safe id the same payloads are recorded.
	const safe = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	for event, handle := range reg.Handlers() {
		if event == "statusline" {
			continue // renders another command's output; it names no session of its own
		}
		for _, id := range append([]string{safe}, unsafe...) {
			payload, _ := json.Marshal(map[string]any{
				"session_id": id, "conversation_id": id, "conversationId": id, "agent_id": id,
				"cwd": root, "workspacePaths": []string{root}, "reason": "exit",
				"file_path": root + "/a.go", "file": root + "/a.go", "tool_name": "Write",
			})
			env := hookrun.Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(string(payload)), Spool: sp, Version: "test"}
			if err := handle(context.Background(), env); err != nil {
				t.Errorf("%s: a hook must never fail: %v", event, err)
			}
		}
	}
	recorded := 0
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.SessionID == safe {
			recorded++
		}
		for _, id := range unsafe {
			if e.SessionID == id {
				t.Errorf("%s spooled with the unsafe session id %q", e.Name, id)
			}
		}
	}
	if recorded == 0 {
		t.Fatal("no handler recorded the safe session either: the payloads were not read")
	}
}
