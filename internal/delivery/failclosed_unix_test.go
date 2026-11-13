//go:build unix

package delivery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// A crossing neither the run ledger nor its sidecar could record, the ledger's lock held
// and its folder not writable, lives on in the mixed placement alone: the run's next hook
// within the placement's lifetime records it, from when it happened. Delivery reads only
// the ledger, so nothing that carries content and was spooled after the crossing is sent:
// neither a reply the next hook spools before it places the session, nor what comes after.
func TestACrossingNeitherTheLedgerNorItsSidecarTookIsRecordedByTheNextHook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only folder")
	}
	t.Parallel()
	m := newCrossingMachine(t)
	dir := filepath.Join(m.state, runs.Dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	crossedAt := m.began.Add(10 * time.Minute)
	m.cross(crossedAt)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, mixed := runs.MixedAt(m.state, crossingSession, time.Time{}); mixed {
		t.Fatal("the crossing hook wrote the ledger or its sidecar through a held lock and a read-only folder")
	}

	// The next hook spools a reply before it places the session, as a turn's end does.
	m.spoolAt("spooled-before-the-retry", m.began.Add(15*time.Minute))
	next, _ := m.hook(m.began.Add(20 * time.Minute))
	hookrun.ClaimFromPayload(context.Background(), next, hookrun.PayloadSession{ID: crossingSession, Cwd: m.side}, "claude-code")
	if at, mixed := runs.MixedAt(m.state, crossingSession, time.Time{}); !mixed || !at.Equal(crossedAt.UTC()) {
		t.Fatalf("after the next hook the ledger reads Mixed %v from %v, want from %v", mixed, at, crossedAt.UTC())
	}
	after, r := m.hook(m.began.Add(30 * time.Minute))
	after.EmitFor(r, toolCall("after-the-retry", time.Time{}))

	m.delivered(m.flush(m.router()), []string{"before-the-crossing"}, []string{"spooled-before-the-retry", "after-the-retry"}, 2)
}
