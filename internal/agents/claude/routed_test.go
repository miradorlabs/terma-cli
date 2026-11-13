package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// On a migrated machine the quota snapshot the status line spools itself carries the
// destination routing gave the session's repository, and no project.
func TestRoutedClaudeQuotaCarriesItsDestination(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	st := hookruntest.TwoOrganizations(t)
	env.Policy, env.Team, env.Routing = config.NoPolicy("", ""), "", hookrun.NewRouting(hookruntest.RoutedProfile, st)
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	if !captureQuota(env, &p) {
		t.Fatal("no snapshot captured")
	}
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 1 || evs[0].Enrollment != hookruntest.Enrollment(t, st, "acme-platform") || evs[0].OwnLocation {
		t.Fatalf("spooled %+v", evs)
	}
	if _, ok := evs[0].Attrs[hookrun.AttrProjectID]; ok {
		t.Errorf("the snapshot names a project: %v", evs[0].Attrs)
	}
}

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
