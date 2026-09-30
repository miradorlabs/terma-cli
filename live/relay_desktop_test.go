package live

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Claude Desktop's Code tab through the relay (claude_desktop.go for the launch): the
// build Desktop pins, started the way Desktop starts it, with the exporter only in
// the user's settings — where `terma relay setup` put it. On one relay, a session in
// the bound repository, one in a worktree Desktop's "use worktree" mode would create
// (<repo>/.claude/worktrees/<name>, a linked worktree), and a personal one: the first
// two reach their project with its key, under Desktop's own service name, and the
// personal one reaches nothing.
func TestRelayClaudeDesktop(t *testing.T) {
	forEachClaudeDesktop(t, func(t *testing.T, b Binary) {
		Proves(t, "claude-desktop", b.Version, "relay.desktop")
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true, Content: true, Hold: 5 * time.Second})
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL

		// The hooks are committed, as once the install is merged: a worktree checks out
		// what is committed.
		sb.git("add", "-A")
		sb.git("commit", "-q", "-m", "install terma")
		wt := filepath.Join(sb.Repo, ".claude", "worktrees", "desk")
		sb.git("worktree", "add", "-q", "-b", "claude/desk", wt)
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}

		repoSID := sb.ClaudeDesktopRun(sb.Repo, telemetryPrompt)
		calls.Store(0)
		wtSID := sb.ClaudeDesktopRun(wt, telemetryPrompt)
		calls.Store(0)
		personalSID := sb.ClaudeDesktopRun(personal, telemetryPrompt)

		deadline := time.Now().Add(30 * time.Second)
		for (len(reached(sb.Receiver)[repoSID]) == 0 || len(reached(sb.Receiver)[wtSID]) == 0) && time.Now().Before(deadline) {
			time.Sleep(time.Second)
		}
		time.Sleep(8 * time.Second) // past the hold: the personal session is dropped by now
		got := reached(sb.Receiver)
		for name, sid := range map[string]string{"repository": repoSID, "worktree": wtSID} {
			if r := got[sid]; !r["project "+sb.ProjectID] || !r["key Bearer "+liveKey] || len(r) != 2 {
				t.Errorf("the %s session %s reached upstream as %v", name, sid, r)
			}
			if len(sb.APIRequests(sid, time.Second)) == 0 {
				t.Errorf("no api_request of the %s session reached upstream", name)
			}
			if len(sb.Events("terma.session.start", sid)) == 0 {
				t.Errorf("the %s session's hooks never announced it: Desktop's launch ran no repository hook", name)
			}
		}
		if r := got[personalSID]; len(r) > 0 {
			t.Errorf("the personal Desktop session reached upstream: %v", r)
		}
		desktop := 0
		for _, l := range sb.Receiver.evidence().logs {
			if l.Resource["service.name"] == "claude-code-desktop" {
				desktop++
			}
		}
		if desktop == 0 {
			t.Errorf("nothing upstream carried Desktop's service name claude-code-desktop")
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if sum(c, "dropped.unclaimed") == 0 {
			t.Errorf("the personal session was never received and dropped, so the control proves nothing: %v", c)
		}
	})
}
