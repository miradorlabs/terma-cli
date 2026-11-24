package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// `terma hook` reads its own process's environment: a Claude Code hook entry grok started
// claims and queues nothing, and the same call without grok's mark claims the session.
func TestHookStandsDownInAProcessGrokStarted(t *testing.T) {
	root := collectedRepo(t)
	payload := `{"session_id":"grok-session","hook_event_name":"SessionStart","cwd":"` + hookruntest.InJSON(root) + `"}`
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("GROK_HOOK_EVENT", "session_start")
	runHook(t, "session-start", payload)
	if c, ok := claim.Read(testApp.stateDir, "grok-session", time.Now()); ok {
		t.Errorf("grok's process claimed the session: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(testApp.stateDir, spool.Dir)); !os.IsNotExist(err) {
		t.Errorf("grok's process queued: %v", err)
	}
	t.Setenv("GROK_HOOK_EVENT", "")
	runHook(t, "session-start", payload)
	if c, ok := claim.Read(testApp.stateDir, "grok-session", time.Now()); !ok || c.Kind != claim.KindRepository || c.Profile != config.DefaultProfile || c.Account == "" {
		t.Errorf("without grok's mark the session was not claimed: %+v %v", c, ok)
	}
}
