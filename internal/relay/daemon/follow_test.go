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
// does the service's relay, nor when a service of this terma waits for the lock already. A
// service that is installed but not waiting (booted out, or past its start limit) is no
// successor.
func TestASupersededRelayIsFollowedUnlessTheServiceTakesOver(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		service, runsThis, waiting bool
		followed                   bool
	}{
		{"hook relay, no service of this terma", false, false, false, true},
		{"hook relay, a service of this terma, not waiting", false, true, false, true},
		{"hook relay, a service of this terma waiting", false, true, true, false},
		{"service relay", true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runsThis(t, tc.runsThis)
			calls := followed(t)
			stateDir, dir, _ := setUpRelay(t)
			if tc.waiting {
				unmark, err := flock.TryLock(filepath.Join(dir, ServiceWaitFile))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(unmark)
			}
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

	waiting := make(chan struct{}, 1)
	was := followWaiting
	followWaiting = func() { waiting <- struct{}{} }
	t.Cleanup(func() { followWaiting = was })

	f := runConfig(stateDir, 0, nil)
	f.Addr, f.Follow = addr, true
	follower := startRun(t, f)
	select {
	case <-waiting:
	case <-follower.done:
		t.Fatalf("the follower gave way with no other waiting: %+v", follower.res)
	case <-time.After(5 * time.Second):
		t.Fatal("the follower never started waiting")
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
	// It polls every followPoll; the rest is starting to listen.
	if took := time.Since(gone); took > 300*time.Millisecond {
		t.Errorf("the follower listened %v after the relay it follows was gone", took)
	}
}

// The service's relay holds ServiceWaitFile while it waits for another relay's lock, and lets
// go once it runs: that is how Supersede knows it will take over.
func TestAWaitingServiceRelayMarksItself(t *testing.T) {
	stateDir, dir, _ := setUpRelay(t)
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile)) // another relay runs
	if err != nil {
		t.Fatal(err)
	}
	c := runConfig(stateDir, 0, nil)
	c.Service = true
	r := startRun(t, c)
	for deadline := time.Now().Add(5 * time.Second); !serviceWaiting(dir); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the waiting service relay never marked itself")
		}
	}
	unlock()
	r.await(t, "the service relay")
	if serviceWaiting(dir) {
		t.Error("the service relay still marks itself waiting once it runs")
	}
}
