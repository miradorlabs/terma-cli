package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// A Mixed run that goes on in one place records nothing new in the run ledger, yet its
// hooks renew the ledger once a day, so the relay's daily prune never ends its Mixed
// state. Renewing costs one write a day: a second hook the same day leaves the ledger
// alone. A Mixed run whose session goes quiet keeps its ledger past runs.Retention, for
// runs.MixedRetention, so a hook that goes on with it weeks later, without a new run, still
// finds it Mixed; only then is it pruned.
func TestALongMixedRunStaysMixed(t *testing.T) {
	t.Parallel()
	for name, working := range map[string]bool{"working": true, "idle": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, acmeAndPersonal(t))
			side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
			const sid = "long-session"
			r := startIn(t, m.hook(), sid, side)
			m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, nil)
			if ledger, _ := m.mixed(sid); !ledger {
				t.Fatal("the run did not turn Mixed")
			}
			start := time.Now()
			renewals, active := 0, time.Time{}
			ledger := filepath.Join(m.stateDir, runs.Dir, sid+".json")
			claimFile := filepath.Join(claim.Dir(m.stateDir), "claims", sid+".json")
			goOn := func(at time.Time) {
				t.Helper()
				// The session's hooks keep its mixed placement fresh, so none of them finds a
				// crossing to record again.
				if err := os.Chtimes(claimFile, at.Add(-time.Minute), at.Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				env := m.hook()
				env.Now = at
				ClaimFromPayload(context.Background(), env, PayloadSession{ID: sid, Cwd: side}, "claude-code")
			}
			for day := 1; day <= 20; day++ {
				now := start.Add(time.Duration(day) * 24 * time.Hour)
				if working {
					for _, at := range []time.Time{now, now.Add(time.Hour)} {
						goOn(at)
						p, _ := runs.Locate(m.stateDir, sid)
						if at.Sub(p.Active) > runs.RefreshEvery {
							t.Fatalf("day %d: the ledger's activity is %v, over a day before the hook at %v", day, p.Active, at)
						}
						if !p.Active.Equal(active) {
							renewals, active = renewals+1, p.Active
						}
					}
				}
				runs.Prune(context.Background(), m.stateDir, now, nil)
				if _, err := os.Stat(ledger); err != nil {
					t.Fatalf("day %d: the Mixed run's ledger was pruned: %v", day, err)
				}
			}
			// 40 hooks over 20 days: renewed about daily, never by every hook.
			if working && (renewals == 0 || renewals > 20) {
				t.Fatalf("the ledger was renewed %d times by 40 hooks over 20 days", renewals)
			}
			// The session goes on after 20 days, without a new run: still Mixed.
			later := start.Add(20 * 24 * time.Hour)
			goOn(later)
			if ledger, placement := m.mixed(sid); !ledger || !placement {
				t.Fatalf("after 20 days the run reads Mixed %v in the ledger, %v in its placement", ledger, placement)
			}
			runs.Prune(context.Background(), m.stateDir, later.Add(runs.MixedRetention-time.Hour), nil)
			if _, err := os.Stat(ledger); err != nil {
				t.Fatalf("pruned within MixedRetention of its last hook: %v", err)
			}
			runs.Prune(context.Background(), m.stateDir, later.Add(runs.MixedRetention+24*time.Hour), nil)
			if _, err := os.Stat(ledger); !os.IsNotExist(err) {
				t.Fatalf("kept past MixedRetention of its last hook: %v", err)
			}
		})
	}
}
