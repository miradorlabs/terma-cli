package delivery

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A crossing the run ledger could not record, its lock held past the hook's wait, is
// recorded by the run's next hook, from when it happened. Delivery reads only the ledger,
// so nothing that carries content and was spooled after the crossing is sent: neither a
// reply the next hook spools before it places the session, nor what comes after.
func TestACrossingTheLedgerMissedIsRecordedByTheNextHook(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	acmeHost, personalHost := newIngest(t), newIngest(t)
	personal := personalTeam()
	personal.Policy.IncludePrompts, personal.Policy.IncludeToolContent = true, true
	e := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/*"), personal}, bindAcme)
	m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", acmeHost)
	m.key(e["personal"], "personal", "ter_srv_personal", personalHost)
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "unrecorded-crossing"
	ctx := context.Background()
	began := time.Now().Add(-time.Hour)
	hook := func(at time.Time) (hookrun.Env, *hookrun.Repo) {
		t.Helper()
		env := hookrun.Env{StateDir: m.state, ConfigDir: m.config, Now: at, Spool: m.spool, Policy: config.NoPolicy("", ""),
			Routing: hookrun.LoadRouting(m.state, config.DefaultProfile)}
		r, ok := env.Open(ctx, sid, side)
		if !ok {
			t.Fatal("routing collects nothing in personal's checkout")
		}
		return env, r
	}
	toolCall := func(mark string, at time.Time) spool.Event {
		return spool.Event{Time: at, Name: semconv.TermaToolCallEvent, SessionID: sid,
			Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "claude-code", semconv.TermaTurnIDKey: mark}}
	}
	sess := session.Session{ID: sid, Tool: "claude-code"}

	start, r := hook(began)
	start.StartRun(sid)
	start.EmitFor(r, toolCall("before-the-crossing", time.Time{}))

	unlock, err := flock.TryLock(filepath.Join(m.state, runs.Dir, sid+".json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	crossedAt := began.Add(10 * time.Minute)
	cross, r := hook(crossedAt)
	cross.Touch(r, sess, "Edit", []string{filepath.Join(api, "main.go")}, nil)
	unlock()
	if _, mixed := runs.MixedAt(m.state, sid, time.Time{}); mixed {
		t.Fatal("the crossing hook wrote the ledger through a held lock")
	}
	if c, ok := claim.Read(m.state, sid, crossedAt); !ok || c.Kind != claim.KindMixed {
		t.Fatalf("the crossing hook's placement %+v", c)
	}

	// The next hook spools a reply before it places the session, as a turn's end does.
	next, r := hook(began.Add(20 * time.Minute))
	if err := m.spool.Append(next.Routed(r, toolCall("spooled-before-the-retry", began.Add(15*time.Minute)))); err != nil {
		t.Fatal(err)
	}
	hookrun.ClaimFromPayload(ctx, next, hookrun.PayloadSession{ID: sid, Cwd: side}, "claude-code")
	if at, mixed := runs.MixedAt(m.state, sid, time.Time{}); !mixed || !at.Equal(crossedAt.UTC()) {
		t.Fatalf("after the next hook the ledger reads Mixed %v from %v, want from %v", mixed, at, crossedAt.UTC())
	}
	after, r := hook(began.Add(30 * time.Minute))
	after.EmitFor(r, toolCall("after-the-retry", time.Time{}))

	res := m.flush(m.router())
	_, sent := personalHost.sent()
	if !strings.Contains(sent, "before-the-crossing") {
		t.Errorf("what came before the crossing was not sent: %+v", res)
	}
	for _, mark := range []string{"spooled-before-the-retry", "after-the-retry"} {
		if strings.Contains(sent, mark) {
			t.Errorf("%s was sent after its run crossed", mark)
		}
	}
	if _, touched := acmeHost.sent(); !strings.Contains(touched, "main.go") {
		t.Error("the crossing edit's file event did not go to Acme by its own checkout")
	}
	if res.Mixed != 2 {
		t.Errorf("Flush = %+v, want 2 dropped as Mixed", res)
	}
}
