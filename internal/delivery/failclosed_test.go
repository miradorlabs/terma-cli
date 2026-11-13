package delivery

import (
	"context"
	"os"
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

// crossingMachine is a migrated machine where Acme Platform collects github.com/acme/* and
// personal, keeping prompts and tool content, everything else, each with its own ingest,
// and a session that starts in personal's checkout an hour ago.
type crossingMachine struct {
	*machine
	acmeHost, personalHost *ingest
	side, api              string
	began                  time.Time
}

const crossingSession = "unrecorded-crossing"

func newCrossingMachine(t *testing.T) *crossingMachine {
	t.Helper()
	m := &crossingMachine{machine: migratedMachine(t), acmeHost: newIngest(t), personalHost: newIngest(t), began: time.Now().Add(-time.Hour)}
	personal := personalTeam()
	personal.Policy.IncludePrompts, personal.Policy.IncludeToolContent = true, true
	e := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/*"), personal}, bindAcme)
	m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", m.acmeHost)
	m.key(e["personal"], "personal", "ter_srv_personal", m.personalHost)
	m.side, m.api = hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	start, r := m.hook(m.began)
	start.StartRun(crossingSession)
	start.EmitFor(r, toolCall("before-the-crossing", time.Time{}))
	return m
}

// hook is a hook of the session in personal's checkout at at.
func (m *crossingMachine) hook(at time.Time) (hookrun.Env, *hookrun.Repo) {
	m.t.Helper()
	env := hookrun.Env{StateDir: m.state, ConfigDir: m.config, Now: at, Spool: m.spool, Policy: config.NoPolicy("", ""),
		Routing: hookrun.LoadRouting(m.state, config.DefaultProfile)}
	r, ok := env.Open(context.Background(), crossingSession, m.side)
	if !ok {
		m.t.Fatal("routing collects nothing in personal's checkout")
	}
	return env, r
}

// cross is the hook that edits Acme's checkout at at, with the run ledger's lock held
// throughout, so no write to the ledger can be made.
func (m *crossingMachine) cross(at time.Time) {
	m.t.Helper()
	unlock, err := flock.TryLock(filepath.Join(m.state, runs.Dir, crossingSession+".json.lock"))
	if err != nil {
		m.t.Fatal(err)
	}
	defer unlock()
	env, r := m.hook(at)
	env.Touch(r, session.Session{ID: crossingSession, Tool: "claude-code"}, "Edit", []string{filepath.Join(m.api, "main.go")}, nil)
	if c, ok := claim.Read(m.state, crossingSession, at); !ok || c.Kind != claim.KindMixed {
		m.t.Fatalf("the crossing hook's placement %+v", c)
	}
}

// spoolAt spools a tool call marked mark at at, as a hook spools a turn's records, without
// placing the session.
func (m *crossingMachine) spoolAt(mark string, at time.Time) {
	m.t.Helper()
	env, r := m.hook(at)
	if err := m.spool.Append(env.Routed(r, toolCall(mark, at))); err != nil {
		m.t.Fatal(err)
	}
}

// delivered checks what a flush sent: every mark in sent to personal's ingest and none of
// dropped, the crossing edit's file event to Acme's by its own checkout, and how many
// records it dropped for the crossing.
func (m *crossingMachine) delivered(res Result, sent, dropped []string, mixed int) {
	m.t.Helper()
	_, body := m.personalHost.sent()
	for _, mark := range sent {
		if !strings.Contains(body, mark) {
			m.t.Errorf("%s was not sent: %+v", mark, res)
		}
	}
	for _, mark := range dropped {
		if strings.Contains(body, mark) {
			m.t.Errorf("%s was sent after its run crossed", mark)
		}
	}
	if _, touched := m.acmeHost.sent(); !strings.Contains(touched, "main.go") {
		m.t.Error("the crossing edit's file event did not go to Acme by its own checkout")
	}
	if res.Mixed != mixed {
		m.t.Errorf("Flush = %+v, want %d dropped as Mixed", res, mixed)
	}
}

func toolCall(mark string, at time.Time) spool.Event {
	return spool.Event{Time: at, Name: semconv.TermaToolCallEvent, SessionID: crossingSession,
		Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "claude-code", semconv.TermaTurnIDKey: mark}}
}

// A crossing the run ledger refuses, its lock held past the hook's wait, is recorded aside,
// in the ledger's sidecar, which never expires. Six hours on, with no hook of the session
// since, its claim long expired and pruned, the run still reads Mixed from the crossing,
// and delivery drops what carries content and was spooled after it. The next hook that
// can lock the ledger folds the sidecar in.
func TestACrossingTheLedgerRefusedOutlivesItsClaim(t *testing.T) {
	t.Parallel()
	m := newCrossingMachine(t)
	crossedAt := m.began.Add(10 * time.Minute)
	m.cross(crossedAt)
	m.spoolAt("spooled-after-the-crossing", crossedAt.Add(5*time.Minute))

	later := time.Now().Add(6 * time.Hour)
	if _, ok := claim.Read(m.state, crossingSession, later); ok {
		t.Fatal("the claim has not expired six hours on")
	}
	claim.Prune(m.state, later)
	if at, mixed := runs.MixedAt(m.state, crossingSession, time.Time{}); !mixed || !at.Equal(crossedAt.UTC()) {
		t.Fatalf("six hours on the run reads Mixed %v from %v, want from %v", mixed, at, crossedAt.UTC())
	}
	m.delivered(m.flush(m.router()), []string{"before-the-crossing"}, []string{"spooled-after-the-crossing"}, 1)

	next, _ := m.hook(later)
	hookrun.ClaimFromPayload(context.Background(), next, hookrun.PayloadSession{ID: crossingSession, Cwd: m.side}, "claude-code")
	if _, err := os.Stat(filepath.Join(m.state, runs.Dir, crossingSession+".mixed")); !os.IsNotExist(err) {
		t.Fatalf("the next hook left the sidecar: %v", err)
	}
	if at, mixed := runs.MixedAt(m.state, crossingSession, time.Time{}); !mixed || !at.Equal(crossedAt.UTC()) {
		t.Fatalf("after the fold the run reads Mixed %v from %v, want from %v", mixed, at, crossedAt.UTC())
	}
}
