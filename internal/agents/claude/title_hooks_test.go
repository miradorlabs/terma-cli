package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const titleSession = "05f4c583-965f-4eee-a5ee-cfe76fd127c4"

func titleRecord(kind, title string) string {
	key := map[string]string{"custom-title": "customTitle", "ai-title": "aiTitle"}[kind]
	b, _ := json.Marshal(map[string]string{"type": kind, key: title, "sessionId": titleSession})
	return string(b)
}

// transcript writes a transcript of lines and returns its path.
func transcript(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	hookruntest.WriteFile(t, dir, titleSession+".jsonl", strings.Join(lines, "\n")+"\n")
	return filepath.Join(dir, titleSession+".jsonl")
}

// titleEnv is a Claude hook whose developer chose Claude Code at setup, with the relay set up.
func titleEnv(t *testing.T) hookrun.Env {
	t.Helper()
	env := newFundingEnv(t)
	env.ConfigDir = t.TempDir()
	path := claim.TokenPath(env.StateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(env.ConfigDir, config.DefaultProfile, func(p *config.Profile) { p.Harnesses = []string{name} }); err != nil {
		t.Fatal(err)
	}
	env.Agents = []string{name}
	return env
}

func stopClaudeTitles(t *testing.T, env hookrun.Env, path string) []spool.Event {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": titleSession, "cwd": env.Cwd, "transcript_path": path, "hook_event_name": "Stop"})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
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

// The desktop app's name, a custom-title, is spooled once for the session it names.
func TestClaudeStopSpoolsTheDesktopTitle(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(),
		`{"type":"user","sessionId":"`+titleSession+`","message":{"content":"hi"}}`,
		titleRecord("custom-title", "Test session"),
		`{"type":"agent-name","agentName":"Test session","sessionId":"`+titleSession+`"}`,
	)
	titles := stopClaudeTitles(t, env, path)
	if len(titles) != 1 {
		t.Fatalf("expected one title, got %d: %+v", len(titles), titles)
	}
	got := titles[0]
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaSessionTitleKey: "Test session",
		semconv.TermaEvidenceSourceKey: sourceClaudeTranscript, hookrun.AttrProjectID: hookruntest.Team,
	} {
		if got.Attrs[k] != want {
			t.Errorf("attrs[%q] = %v (%T), want %v", k, got.Attrs[k], got.Attrs[k], want)
		}
	}
	if got.SessionID != titleSession {
		t.Errorf("session %q, want the session the title names", got.SessionID)
	}
	// Claude Code writes the record again every turn; an unchanged name is not sent again.
	if again := stopClaudeTitles(t, env, path); len(again) != 0 {
		t.Fatalf("title spooled twice: %+v", again)
	}
}

// A /rename after the last turn ends no turn: the session's end sends it.
func TestClaudeSessionEndSpoolsALastRename(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(), titleRecord("ai-title", "Integration tests"), titleRecord("custom-title", "Renamed"))
	b, _ := json.Marshal(map[string]any{"session_id": titleSession, "cwd": env.Cwd, "transcript_path": path, "hook_event_name": "SessionEnd"})
	env.Stdin = strings.NewReader(string(b))
	if err := sessionEnd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	var titles []spool.Event
	for _, e := range hookruntest.Spooled(t, env.Spool) {
		if e.Name == semconv.TermaSessionTitleEvent {
			titles = append(titles, e)
		}
	}
	if len(titles) != 1 || titles[0].Attrs[semconv.TermaSessionTitleKey] != "Renamed" {
		t.Fatalf("expected the rename at the session's end, got %+v", titles)
	}
}

// A rename outranks the generated title and is a new event; an emptied one falls back to it.
func TestClaudeRenameSpoolsTheNewName(t *testing.T) {
	env := titleEnv(t)
	dir := t.TempDir()
	for _, c := range []struct {
		lines []string
		want  string
	}{
		{[]string{titleRecord("ai-title", "Integration tests")}, "Integration tests"},
		{[]string{titleRecord("ai-title", "Integration tests"), titleRecord("custom-title", "Renamed"), titleRecord("ai-title", "Integration tests")}, "Renamed"},
		{[]string{titleRecord("custom-title", "Renamed"), titleRecord("custom-title", ""), titleRecord("ai-title", "Integration tests")}, "Integration tests"},
	} {
		titles := stopClaudeTitles(t, env, transcript(t, dir, c.lines...))
		if len(titles) != 1 || titles[0].Attrs[semconv.TermaSessionTitleKey] != c.want {
			t.Fatalf("expected %q, got %+v", c.want, titles)
		}
	}
}

// Before a title is written nothing is spooled or remembered; another session's, a blank or a
// broken line is no title.
func TestClaudeTitleNotYetWritten(t *testing.T) {
	env := titleEnv(t)
	dir := t.TempDir()
	if titles := stopClaudeTitles(t, env, filepath.Join(dir, "missing.jsonl")); len(titles) != 0 {
		t.Fatalf("no transcript, yet spooled %+v", titles)
	}
	path := transcript(t, dir,
		`{"type":"custom-title","customTitle":"Another session","sessionId":"0b0e6c34-1c8e-4c5c-9a55-0ad6a3c7d1e2"}`,
		titleRecord("ai-title", "   "), `{"type":"custom-title",`,
	)
	if titles := stopClaudeTitles(t, env, path); len(titles) != 0 {
		t.Fatalf("spooled %+v", titles)
	}
	path = transcript(t, dir, titleRecord("ai-title", "Integration tests"))
	if titles := stopClaudeTitles(t, env, path); len(titles) != 1 {
		t.Fatalf("the title written since was not sent: %+v", titles)
	}
}

// Only the transcript's tail is read: the title Claude Code wrote again near the end is found
// past a large transcript, one starting mid-record included.
func TestClaudeTitleReadsTheTail(t *testing.T) {
	dir := t.TempDir()
	big := `{"type":"user","sessionId":"` + titleSession + `","message":{"content":"` + strings.Repeat("x", claudeTitleTail) + `"}}`
	path := transcript(t, dir, titleRecord("custom-title", "Scrolled out"), big, titleRecord("ai-title", "Near the end"))
	title, found, err := readTranscriptTitle(path, titleSession)
	if err != nil || !found || title != "Near the end" {
		t.Fatalf("readTranscriptTitle = %q, %v, %v; want the title in the tail", title, found, err)
	}
}

// The name restates the first prompt: it travels under the consent prompts do.
func TestClaudeTitleNeedsTheConsentPromptsTravelUnder(t *testing.T) {
	for _, c := range []struct {
		name     string
		env      func(t *testing.T) hookrun.Env
		withheld bool
		want     int
	}{
		{"the developer did not choose Claude Code", newFundingEnv, false, 0},
		{"Claude Code chosen, the team withholds prompts", titleEnv, true, 0},
		{"Claude Code chosen at setup", titleEnv, false, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := c.env(t)
			env.Policy.IncludePrompts = !c.withheld
			titles := stopClaudeTitles(t, env, transcript(t, t.TempDir(), titleRecord("ai-title", "Integration tests")))
			r := delivery.Router{ConfigDir: env.ConfigDir, Consent: func(_ string, c hookrun.Consent) bool { return Agent{}.ContentConsented(c) }}
			var sent int
			for _, e := range titles {
				if _, ok := r.Outgoing(env.Policy, hookruntest.Team, e); ok {
					sent++
				}
			}
			if sent != c.want {
				t.Fatalf("delivered %d titles, want %d", sent, c.want)
			}
		})
	}
}

// A rename has no length limit of its own; the title is cut on a rune boundary.
func TestClaudeTitleIsBounded(t *testing.T) {
	env := titleEnv(t)
	long := strings.Repeat("é", claudeTitleMaxText)
	titles := stopClaudeTitles(t, env, transcript(t, t.TempDir(), titleRecord("custom-title", long)))
	if len(titles) != 1 {
		t.Fatalf("titles: %+v", titles)
	}
	got := titles[0].Attrs[semconv.TermaSessionTitleKey].(string)
	if len(got) > claudeTitleMaxText || !strings.HasPrefix(long, got) || len(got)%2 != 0 {
		t.Fatalf("title cut to %d bytes, not on a rune boundary within %d", len(got), claudeTitleMaxText)
	}
}
