package claude

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A session start, a resume or a clear begins a new run; a compaction continues the run it
// compacts, so it never ends a Mixed one.
func TestRoutedClaudeCompactionKeepsItsRun(t *testing.T) {
	root := newRepo(t)
	stateDir := t.TempDir()
	sp, _ := spool.Open(t.TempDir())
	st := hookruntest.TwoOrganizations(t)
	start := func(source string) string {
		t.Helper()
		env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Policy: config.NoPolicy("", ""),
			Routing: hookrun.NewRouting(hookruntest.RoutedProfile, st),
			Stdin:   strings.NewReader(`{"session_id":"sess-runs","cwd":"` + hookruntest.InJSON(root) + `","hook_event_name":"SessionStart","source":"` + source + `"}`)}
		if err := sessionStart(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		starts := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaSessionStartEvent)
		if len(starts) != 1 || starts[0].Run == "" {
			t.Fatalf("%s: session starts %+v", source, starts)
		}
		return starts[0].Run
	}
	first := start("startup")
	if got := start("compact"); got != first {
		t.Errorf("a compaction began run %s, want %s continued", got, first)
	}
	if got := start("resume"); got == first {
		t.Error("a resume continued the run")
	}
}
