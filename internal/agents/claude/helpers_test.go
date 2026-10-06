package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// newRepo is a repository with private terma and Claude Code config directories.
func newRepo(t *testing.T) string {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return hookruntest.InitRepo(t)
}

// newFundingEnv is a hook environment in a repository bound to project-a, with no developer
// credential leaking in.
func newFundingEnv(t *testing.T) hookrun.Env {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	root, stateDir, sp := hookruntest.Project(t)
	return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Version: "test", Policy: hookruntest.Admitting(root), Team: hookruntest.Team}
}

func hookPayload(env hookrun.Env, event string) string {
	b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "hook_event_name": event})
	return string(b)
}

func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The reader refuses a payload past the bound by name.
func TestReaderRefusesOversizedInput(t *testing.T) {
	t.Parallel()
	const valid = `{"session_id":"valid"}`
	if _, err := readClaudeInput(strings.NewReader(valid)); err != nil {
		t.Fatalf("a payload within the bound was refused: %v", err)
	}
	if _, err := readClaudeInput(strings.NewReader(valid + strings.Repeat(" ", hookrun.MaxInput))); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized payload: err = %v, want too large", err)
	}
}

// appendLogin appends to the transcript a /login the session ran itself, as Claude Code records it.
func appendLogin(t *testing.T, transcript, sessionID string, at time.Time) {
	t.Helper()
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintf(f, `{"type":"user","message":{"role":"user","content":"<command-name>/login</command-name>\n<command-message>login</command-message>\n<command-args></command-args>"},"timestamp":%q,"sessionId":%q}`+"\n",
		at.UTC().Format(time.RFC3339Nano), sessionID); err != nil {
		t.Fatal(err)
	}
}
