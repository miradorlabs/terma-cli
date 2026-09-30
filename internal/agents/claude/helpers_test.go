package claude

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
)

// newRepo is a repository with private terma and Claude Code config directories.
func newRepo(t *testing.T) string {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return hookruntest.InitRepo(t)
}

// newFundingEnv is a hook environment in a repository bound to project-a, where no
// credential of the developer's leaks in from the environment.
func newFundingEnv(t *testing.T) hookrun.Env {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	root, sp := hookruntest.Project(t)
	return hookrun.Env{Now: time.Now(), Cwd: root, Spool: sp, Version: "test", Policy: config.DefaultPolicy()}
}

// hookPayload is a minimal hook payload for event in env's repository.
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
	const valid = `{"session_id":"valid"}`
	if _, err := readClaudeInput(strings.NewReader(valid)); err != nil {
		t.Fatalf("a payload within the bound was refused: %v", err)
	}
	if _, err := readClaudeInput(strings.NewReader(valid + strings.Repeat(" ", hookrun.MaxInput))); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized payload: err = %v, want too large", err)
	}
}
