package codex

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// titleIndex writes session_index.jsonl: one line per naming, renames appended, other
// threads interleaved.
func titleIndex(t *testing.T, lines ...string) {
	t.Helper()
	hookruntest.WriteFile(t, os.Getenv("CODEX_HOME"), "session_index.jsonl", strings.Join(lines, "\n")+"\n")
}

func titleLine(id, name, at string) string {
	b, _ := json.Marshal(map[string]string{"id": id, "thread_name": name, "updated_at": at})
	return string(b)
}

func stopCodexTitles(t *testing.T, env hookrun.Env) []spool.Event {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "model": "gpt-6-sol"})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	var titles []spool.Event
	for _, e := range hookruntest.Spooled(t, env.Spool) {
		if e.Name == hookrun.EventSessionTitle {
			titles = append(titles, e)
		}
	}
	return titles
}

// A turn's end spools the thread's name once, for the session it names, stamped with when
// Codex wrote it.
func TestCodexStopSpoolsTheThreadName(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t)
	titleIndex(t,
		titleLine("01a0e2e8-724f-7ea3-8088-5da1bb9504ec", "Another thread", "2026-09-27T12:48:06.533494Z"),
		titleLine(replySession, "Rate this project", "2026-09-27T12:53:23.735966Z"),
	)

	titles := stopCodexTitles(t, env)
	if len(titles) != 1 {
		t.Fatalf("expected one title, got %d: %+v", len(titles), titles)
	}
	got := titles[0]
	for k, want := range map[string]any{
		"tool": codexTool, "title": "Rate this project", "evidence_source": "codex_session_index",
		"schema_version": float64(1), hookrun.AttrProjectID: "project-a", "terma.version": "test",
	} {
		if got.Attrs[k] != want {
			t.Errorf("attrs[%q] = %v (%T), want %v", k, got.Attrs[k], got.Attrs[k], want)
		}
	}
	if got.SessionID != replySession || !got.Time.Equal(time.Date(2026, 9, 27, 12, 53, 23, 735_966_000, time.UTC)) {
		t.Errorf("session %q at %v: a title is stamped with when Codex wrote it, in the session it names", got.SessionID, got.Time)
	}
	// An unchanged name is not sent again.
	if again := stopCodexTitles(t, env); len(again) != 0 {
		t.Fatalf("title spooled twice: %+v", again)
	}
}

// The latest rename wins and is a new event, even if an earlier line sorts after it.
func TestCodexRenameSpoolsTheNewName(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t)
	titleIndex(t, titleLine(replySession, "Generated title", "2026-09-27T07:09:44.114286Z"))
	if first := stopCodexTitles(t, env); len(first) != 1 {
		t.Fatalf("first title: %+v", first)
	}
	titleIndex(t,
		titleLine(replySession, "Generated title", "2026-09-27T07:09:44.114286Z"),
		titleLine(replySession, "Renamed", "2026-09-27T07:09:48.453777Z"),
		titleLine(replySession, "Stale", "2026-09-27T07:09:40Z"),
	)
	titles := stopCodexTitles(t, env)
	if len(titles) != 1 || titles[0].Attrs["title"] != "Renamed" {
		t.Fatalf("expected the rename, got %+v", titles)
	}
}

// An unnamed thread spools and remembers nothing, so a later turn sends the name once it
// exists.
func TestCodexTitleNotYetWritten(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t)
	if titles := stopCodexTitles(t, env); len(titles) != 0 {
		t.Fatalf("no index file, yet spooled %+v", titles)
	}
	titleIndex(t, titleLine("01a0e2e8-724f-7ea3-8088-5da1bb9504ec", "Another thread", "2026-09-27T12:48:06Z"),
		`{"id":"`+replySession+`","thread_name":"   ","updated_at":"2026-09-27T12:53:00Z"}`, `not json`)
	if titles := stopCodexTitles(t, env); len(titles) != 0 {
		t.Fatalf("another thread's, a blank or a broken line spooled %+v", titles)
	}
	titleIndex(t, titleLine(replySession, "Rate this project", "2026-09-27T12:53:23Z"))
	if titles := stopCodexTitles(t, env); len(titles) != 1 {
		t.Fatalf("the name written since was not sent: %+v", titles)
	}
}

// The name restates the first prompt: it travels under the consent a reply does.
func TestCodexTitleNeedsTheConsentPromptsTravelUnder(t *testing.T) {
	for _, c := range []struct {
		name     string
		setup    func(t *testing.T)
		withheld bool
		want     int
	}{
		{"nothing exports Codex here at all", func(*testing.T) {}, false, 0},
		{"this repository routes Codex, the team withholds prompts", routeCodex, true, 0},
		{"this repository routes Codex", routeCodex, false, 1},
		{"a machine-wide connect, no routing", connectCodexMachineWide, false, 1},
		{"a machine-wide connect, the team withholds prompts", connectCodexMachineWide, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := fundingEnv(t)
			env.Policy.IncludePrompts = !c.withheld
			c.setup(t)
			titleIndex(t, titleLine(replySession, "Rate this project", "2026-09-27T12:53:23Z"))
			// The hook knows no policy; delivery withholds a title the team's prompts-off does.
			if got := delivered(env.Policy, stopCodexTitles(t, env)); len(got) != c.want {
				t.Fatalf("delivered %d titles, want %d", len(got), c.want)
			}
		})
	}
}

// A rename has no length limit of its own; the title is cut on a rune boundary.
func TestCodexTitleIsBounded(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t)
	long := strings.Repeat("é", codexTitleMaxText)
	titleIndex(t, titleLine(replySession, long, "2026-09-27T12:53:23Z"))
	titles := stopCodexTitles(t, env)
	if len(titles) != 1 {
		t.Fatalf("titles: %+v", titles)
	}
	got := titles[0].Attrs["title"].(string)
	if len(got) > codexTitleMaxText || !strings.HasPrefix(long, got) || len(got)%2 != 0 {
		t.Fatalf("title cut to %d bytes, not on a rune boundary within %d", len(got), codexTitleMaxText)
	}
}
