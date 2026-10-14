package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// titleEnv is a Copilot hook whose developer chose Copilot at setup, with COPILOT_HOME a
// directory of the test's own.
func titleEnv(t *testing.T) (hookrun.Env, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("COPILOT_HOME", home)
	root, stateDir, sp := hookruntest.Project(t)
	return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Version: "test", Policy: hookruntest.Admitting(root),
		Team: hookruntest.Team, Agents: []string{name}}, home
}

// workspaceYAML writes the session's workspace.yaml as the runtime does (CLI 1.0.96), its
// name the given YAML scalar.
func workspaceYAML(t *testing.T, home, session, nameScalar string, userNamed bool) {
	t.Helper()
	named := "false"
	if userNamed {
		named = "true"
	}
	hookruntest.WriteFile(t, filepath.Join(home, "session-state", session), "workspace.yaml",
		"id: "+session+"\ncwd: /work/app\nbranch: main\nclient_name: github/cli\nname: "+nameScalar+"\nuser_named: "+named+
			"\nsummary_count: 0\ncreated_at: 2026-10-14T16:44:28.736Z\nupdated_at: 2026-10-14T16:44:31.561Z\n")
}

// endTitles runs the hook of event (Stop or SessionEnd) and returns the titles it spooled.
func endTitles(t *testing.T, env hookrun.Env, event string) []spool.Event {
	t.Helper()
	env.Stdin = strings.NewReader(payload(event, env.Cwd, ""))
	handler := stop
	if event == "SessionEnd" {
		handler = sessionEnd
	}
	if err := handler(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	var titles []spool.Event
	for _, e := range hookruntest.Spooled(t, env.Spool) {
		if e.Name == semconv.TermaSessionTitleEvent {
			titles = append(titles, e)
		}
	}
	return titles
}

// The runtime names a session after its first prompt: the turn's end spools that name once,
// and a rename is spooled again, at the session's end too.
func TestCopilotSpoolsTheSessionsName(t *testing.T) {
	env, home := titleEnv(t)
	workspaceYAML(t, home, copilotSession, `'Create a file named note.txt, then reply with: done'`, false)
	titles := endTitles(t, env, "Stop")
	if len(titles) != 1 {
		t.Fatalf("expected one title, got %+v", titles)
	}
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: name, semconv.TermaSessionTitleKey: "Create a file named note.txt, then reply with: done",
		semconv.TermaEvidenceSourceKey: sourceCopilotWorkspace, hookrun.AttrProjectID: hookruntest.Team,
	} {
		if titles[0].Attrs[k] != want {
			t.Errorf("attrs[%q] = %v, want %v", k, titles[0].Attrs[k], want)
		}
	}
	if titles[0].SessionID != copilotSession {
		t.Errorf("session %q, want %q", titles[0].SessionID, copilotSession)
	}
	if again := endTitles(t, env, "Stop"); len(again) != 0 {
		t.Fatalf("an unchanged name was spooled again: %+v", again)
	}
	workspaceYAML(t, home, copilotSession, "Release notes", true)
	titles = endTitles(t, env, "SessionEnd")
	if len(titles) != 1 || titles[0].Attrs[semconv.TermaSessionTitleKey] != "Release notes" {
		t.Fatalf("expected the rename at the session's end, got %+v", titles)
	}
}

// A first prompt of several lines is the name as the runtime writes it, a block scalar; a long
// one is cut to the bound on a character's edge.
func TestCopilotTitleReadsWhatTheRuntimeWrites(t *testing.T) {
	env, home := titleEnv(t)
	workspaceYAML(t, home, copilotSession, "|-\n  Fix the build\n  then run the tests", false)
	if titles := endTitles(t, env, "Stop"); len(titles) != 1 || titles[0].Attrs[semconv.TermaSessionTitleKey] != "Fix the build\nthen run the tests" {
		t.Fatalf("got %+v", titles)
	}
	long := strings.Repeat("é", hookrun.TitleMaxBytes)
	workspaceYAML(t, home, copilotSession, "'"+long+"'", false)
	titles := endTitles(t, env, "Stop")
	if len(titles) != 1 {
		t.Fatalf("expected the long name, got %+v", titles)
	}
	got, _ := titles[0].Attrs[semconv.TermaSessionTitleKey].(string)
	if len(got) > hookrun.TitleMaxBytes || !strings.HasPrefix(long, got) || got == "" {
		t.Fatalf("a long name left as %d bytes: %q", len(got), got)
	}
}

// Nothing is spooled before the runtime writes a name, for another session's file, a blank or
// a broken one, or for a developer who did not choose Copilot.
func TestCopilotTitleNotYetWritten(t *testing.T) {
	env, home := titleEnv(t)
	if titles := endTitles(t, env, "Stop"); len(titles) != 0 {
		t.Fatalf("no workspace, yet spooled %+v", titles)
	}
	hookruntest.WriteFile(t, filepath.Join(home, "session-state", copilotSession), "workspace.yaml", "id: 0b0e6c34-1c8e-4c5c-9a55-0ad6a3c7d1e2\nname: Another session\n")
	if titles := endTitles(t, env, "Stop"); len(titles) != 0 {
		t.Fatalf("another session's name was spooled: %+v", titles)
	}
	for _, broken := range []string{"''", "'unterminated"} {
		workspaceYAML(t, home, copilotSession, broken, false)
		if titles := endTitles(t, env, "Stop"); len(titles) != 0 {
			t.Fatalf("name %s spooled %+v", broken, titles)
		}
	}
	workspaceYAML(t, home, copilotSession, "Written since", false)
	unchosen := env
	unchosen.Agents = nil
	if titles := endTitles(t, unchosen, "Stop"); len(titles) != 0 {
		t.Fatalf("spooled for a developer who did not choose Copilot: %+v", titles)
	}
	if titles := endTitles(t, env, "Stop"); len(titles) != 1 {
		t.Fatalf("the name written since was not sent: %+v", titles)
	}
}

// Delivery asks the agent again: a title leaves only for a developer who chose Copilot, or in
// global mode.
func TestCopilotTitleConsent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		consent hookrun.Consent
		want    bool
	}{
		{hookrun.Consent{Agents: []string{name}}, true},
		{hookrun.Consent{Global: true}, true},
		{hookrun.Consent{Agents: []string{"claude"}}, false},
	} {
		if got := (Agent{}).ContentConsented(c.consent); got != c.want {
			t.Errorf("ContentConsented(%+v) = %v, want %v", c.consent, got, c.want)
		}
	}
}

// A workspace.yaml past the read's bound is not read: no title, and the hook goes on.
func TestCopilotTitleSkipsAnOversizedWorkspace(t *testing.T) {
	env, home := titleEnv(t)
	workspaceYAML(t, home, copilotSession, "'"+strings.Repeat("x", workspaceMaxBytes)+"'", false)
	if titles := endTitles(t, env, "Stop"); len(titles) != 0 {
		t.Fatalf("an oversized workspace spooled %+v", titles)
	}
}

// The state that keeps an unchanged name from being sent again holds a hash of it, never the
// name, which restates the first prompt.
func TestCopilotTitleStateKeepsNoName(t *testing.T) {
	env, home := titleEnv(t)
	workspaceYAML(t, home, copilotSession, "'a private first prompt'", false)
	if titles := endTitles(t, env, "Stop"); len(titles) != 1 {
		t.Fatalf("expected the title, got %+v", titles)
	}
	err := filepath.WalkDir(filepath.Join(env.StateDir, copilotTitleStateDir), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, _ := os.ReadFile(path); strings.Contains(string(b), "private first prompt") {
			t.Errorf("%s keeps the name", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
