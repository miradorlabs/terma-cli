package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The relay prunes the run ledger with its sweep, keeping every session a record still
// queued names, in the spool or its own outbox: only an idle session nothing names goes.
func TestTheRelaysSweepPrunesTheLedgerOfWhatIsNotQueued(t *testing.T) {
	t.Parallel()
	stateDir, _, _ := setUpRelay(t)
	long := time.Now().Add(-runs.Retention - 24*time.Hour)
	for _, s := range []string{"idle", "spooled", "relayed"} {
		if _, err := runs.Start(stateDir, s, long); err != nil {
			t.Fatal(err)
		}
		if err := runs.MarkMixed(stateDir, s, long); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(stateDir, runs.Dir, s+".json")
		if err := os.Chtimes(path, long, long); err != nil {
			t.Fatal(err)
		}
	}
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Append(spool.Event{Name: "terma.file_touched", SessionID: "spooled"}); err != nil {
		t.Fatal(err)
	}
	outbox := filepath.Join(claim.Dir(stateDir), relay.OutboxDir)
	queued := filepath.Join(outbox, ".teams", "e-acme", "claude-code")
	if err := os.MkdirAll(queued, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queued, "0000000000000000001-000001-logs-1.pb"), []byte(`{"enrollment":"e-acme","session":"relayed"}`+"\nx"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := relay.New(relay.Options{Dir: outbox})
	if got, err := r.QueuedSessions(); err != nil || !got["relayed"] {
		t.Fatalf("the outbox names %v, %v: the test's part is not one it reads", got, err)
	}
	pruneRuns(stateDir, r, time.Now())
	for s, kept := range map[string]bool{"idle": false, "spooled": true, "relayed": true} {
		if _, mixed := runs.MixedAt(stateDir, s, time.Now()); mixed != kept {
			t.Errorf("%s: ledger kept %v, want %v", s, mixed, kept)
		}
	}
}
