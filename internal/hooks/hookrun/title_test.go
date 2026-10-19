package hookrun

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A session's name is spooled once per change: not before the agent has named the session,
// not again while the name stays the same, and not when it cannot be read. It is cut to
// TitleMaxBytes on a character's edge, and carries the agent's attributes and the project.
func TestSpoolTitleSendsEachNameOnce(t *testing.T) {
	t.Parallel()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	sp, _ := spool.Open(t.TempDir())
	env := Env{StateDir: t.TempDir(), Now: time.Now(), Cwd: dir, Spool: sp, Policy: config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p-default"}}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", TitleMaxBytes) // two bytes each: the cut falls inside one
	attrs := map[string]any{"agent": "copilot"}
	for _, read := range []struct {
		title string
		found bool
		err   error
	}{
		{"", false, nil},
		{"Fix the build", true, nil},
		{"Fix the build", true, nil},
		{"", false, errors.New("unreadable")},
		{long, true, nil},
		{long + "more", true, nil}, // the same name once cut
	} {
		SpoolTitle(env, r, "titles", "sess-title-1", attrs, func() (string, bool, error) { return read.title, read.found, read.err })
	}
	events := hookruntest.Spooled(t, sp)
	if len(events) != 2 {
		t.Fatalf("spooled %d titles, want 2: %+v", len(events), events)
	}
	want := []string{"Fix the build", strings.Repeat("é", TitleMaxBytes/2)}
	for i, ev := range events {
		if ev.Name != semconv.TermaSessionTitleEvent || ev.SessionID != "sess-title-1" {
			t.Errorf("event %d is %s for %s", i, ev.Name, ev.SessionID)
		}
		if got := ev.Attrs[semconv.TermaSessionTitleKey]; got != want[i] {
			t.Errorf("title %d = %q, want %q", i, got, want[i])
		}
		if ev.Attrs["agent"] != "copilot" || ev.Attrs[AttrProjectID] != "p-default" {
			t.Errorf("title %d carries %+v", i, ev.Attrs)
		}
	}
	if _, ok := attrs[semconv.TermaSessionTitleKey]; ok {
		t.Error("SpoolTitle wrote into the caller's attributes")
	}
}

func TestTruncateBytesCutsOnACharactersEdge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exact", 5, "exact"},
		{"abcdef", 3, "abc"},
		{"aé", 2, "a"}, // é is two bytes: cutting at 2 would split it
		{"日本", 4, "日"}, // three bytes each
		{"é", 1, ""},
	} {
		if got := TruncateBytes(tc.s, tc.n); got != tc.want {
			t.Errorf("TruncateBytes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
