package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The relay prunes the run ledger with its sweep, keeping every session a record still
// queued names, in the spool or its own outbox: only an idle session nothing names goes,
// here Mixed and idle past MixedRetention.
func TestTheRelaysSweepPrunesTheLedgerOfWhatIsNotQueued(t *testing.T) {
	t.Parallel()
	stateDir, _, _ := setUpRelay(t)
	long := time.Now().Add(-runs.MixedRetention - 24*time.Hour)
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
	pruneRuns(t.Context(), stateDir, r, time.Now())
	for s, kept := range map[string]bool{"idle": false, "spooled": true, "relayed": true} {
		if _, mixed := runs.MixedAt(stateDir, s, time.Now()); mixed != kept {
			t.Errorf("%s: ledger kept %v, want %v", s, mixed, kept)
		}
	}
}

// The relay prunes beside its watch loop: a prune that takes as long as the relay runs
// delays no stop, replacement or idle exit, and stops once the relay does.
func TestASlowPruneDelaysNoDecisionToStop(t *testing.T) {
	t.Parallel()
	for name, request := range map[string]string{"stop": StopFile, "replace": ReplaceFile, "idle": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stateDir, dir, _ := setUpRelay(t)
			idle := time.Hour
			if request == "" {
				idle = time.Millisecond
			}
			c := runConfig(stateDir, idle, nil)
			pruning := make(chan struct{}, 1)
			c.pruneEvery = time.Millisecond
			c.prune = func(ctx context.Context, _ string, _ *relay.Relay) {
				select {
				case pruning <- struct{}{}:
				default:
				}
				<-ctx.Done() // as slow as the relay lets it be
			}
			c.Listening = func(net.Addr, time.Duration) {
				go func() {
					<-pruning
					if request != "" {
						_ = config.WriteFileAtomicNoSync(filepath.Join(dir, request), fmt.Appendf(nil, "%d\n", os.Getpid()), 0o600)
					}
				}()
			}
			began := time.Now()
			res := run(t, c)
			if took := time.Since(began); took > 5*time.Second {
				t.Fatalf("the relay took %v to stop", took)
			}
			if res.Replaced != (request == ReplaceFile) || res.Updated || res.SetupGone {
				t.Fatalf("Run = %+v", res)
			}
		})
	}
}
