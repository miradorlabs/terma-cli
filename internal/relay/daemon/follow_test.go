package daemon

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// followed records the directories Supersede starts a follower for, in place of starting one.
func followed(t *testing.T) func() []string {
	var mu sync.Mutex
	var dirs []string
	was := follow
	follow = func(dir string) {
		mu.Lock()
		dirs = append(dirs, dir)
		mu.Unlock()
	}
	t.Cleanup(func() { follow = was })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(dirs)
	}
}

// A relay asked to make way is followed by the relay that takes its place, since the earlier
// release stepping aside starts none; not when the service manager starts the next, as it
// does the service's relay, and as a service that runs this terma does from the lock.
func TestASupersededRelayIsFollowedUnlessTheServiceTakesOver(t *testing.T) {
	for _, tc := range []struct {
		name              string
		service, runsThis bool
		followed          bool
	}{
		{"hook relay, no service of this terma", false, false, true},
		{"hook relay, a service of this terma", false, true, false},
		{"service relay", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runsThis(t, tc.runsThis)
			calls := followed(t)
			stateDir, dir, _ := setUpRelay(t)
			c := runConfig(stateDir, 0, nil)
			c.Version, c.Service = "1.2.0", tc.service
			r := startRun(t, c)
			awaitRecord(t, r, dir)
			Spawn(stateDir, "1.3.0")
			var want []string
			if tc.followed {
				want = []string{claim.Dir(stateDir)}
			}
			if got := calls(); !slices.Equal(got, want) {
				t.Fatalf("followers started for %v, want %v", got, want)
			}
		})
	}
}

// A follower waits while the relay it follows runs, alone: a second one gives way at once.
// It listens as soon as the relay it follows has stepped aside.
func TestAFollowerTakesOverOnceTheRelayIsGone(t *testing.T) {
	stateDir, dir, _ := setUpRelay(t)
	addr := freeAddr(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr = addr
	old := startRun(t, c)
	awaitRecord(t, old, dir)

	f := runConfig(stateDir, 0, nil)
	f.Addr, f.Follow = addr, true
	follower := startRun(t, f)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		unlock, err := flock.TryLock(filepath.Join(dir, FollowLockFile))
		if flock.IsBusy(err) {
			break // the follower waits
		}
		if err == nil {
			unlock()
		}
		if time.Now().After(deadline) {
			t.Fatal("the follower never started waiting")
		}
	}
	if res := run(t, f); !res.AlreadyRunning {
		t.Fatalf("a second follower = %+v, want it to give way", res)
	}
	select {
	case <-follower.up:
		t.Fatal("the follower listened while the relay it follows ran")
	case <-follower.done:
		t.Fatalf("the follower gave up while the relay it follows ran: %+v", follower.res)
	case <-time.After(time.Second):
	}

	askToMakeWay(t, dir)
	select {
	case <-old.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never stepped aside")
	}
	gone := time.Now()
	follower.await(t, "the follower")
	if took := time.Since(gone); took > time.Second {
		t.Errorf("the follower listened %v after the relay it follows was gone", took)
	}
}
