package live

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// told records the nudges a test's waits gave.
type told struct {
	mu   sync.Mutex
	sent []string
}

func (n *told) Notify(message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, message)
}

func (n *told) messages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

// await runs AwaitAnswer for wait id, its result on the channel.
func await(t *testing.T, path, id string, n Notifier) <-chan bool {
	done := make(chan bool, 1)
	go func() { done <- AwaitAnswer(t.Context(), path, id, n) }()
	return done
}

// A wait still going on two minutes after it began nudges the developer, once.
func TestAwaitAnswerNudgesOnceAtTwoMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "waiting", "s1.json")
		n := &told{}
		if err := StartWait(path, Wait{ID: "w1", Since: time.Now(), Agent: "Agent", Checkout: "shop"}); err != nil {
			t.Fatal(err)
		}
		done := await(t, path, "w1", n)
		time.Sleep(AnswerWait - time.Second)
		synctest.Wait()
		if got := n.messages(); len(got) != 0 {
			t.Fatalf("nudged before two minutes: %q", got)
		}
		time.Sleep(time.Second)
		if !<-done {
			t.Fatal("a wait of two minutes was not nudged")
		}
		if got := n.messages(); len(got) != 1 || got[0] != "Agent is waiting for your answer · shop · 2 min" {
			t.Fatalf("nudged %q", got)
		}
		if nudge(path, "w1", time.Now().Add(time.Hour), n) || len(n.messages()) != 1 {
			t.Fatalf("one wait nudged twice: %q", n.messages())
		}
	})
}

// A wait the developer ended in time never nudges, and leaves nothing behind.
func TestAwaitAnswerNeverNudgesAnEndedWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "waiting")
		path := filepath.Join(dir, "s1.json")
		n := &told{}
		if err := StartWait(path, Wait{ID: "w1", Since: time.Now(), Agent: "Agent"}); err != nil {
			t.Fatal(err)
		}
		done := await(t, path, "w1", n)
		time.Sleep(time.Minute)
		EndWait(path)
		time.Sleep(AnswerWait)
		if <-done || len(n.messages()) != 0 {
			t.Fatalf("an ended wait nudged: %q", n.messages())
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("an ended wait left %v", entries)
		}
	})
}

// A session that waits again starts a new wait: the check for the first nudges about
// neither, and the second's nudges two minutes after it began.
func TestAwaitAnswerNudgesOnlyItsOwnWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s1.json")
		n := &told{}
		if err := StartWait(path, Wait{ID: "w1", Since: time.Now(), Agent: "Agent"}); err != nil {
			t.Fatal(err)
		}
		first := await(t, path, "w1", n)
		time.Sleep(time.Minute)
		if err := StartWait(path, Wait{ID: "w2", Since: time.Now(), Agent: "Agent"}); err != nil {
			t.Fatal(err)
		}
		second := await(t, path, "w2", n)
		time.Sleep(time.Minute)
		if <-first || len(n.messages()) != 0 {
			t.Fatalf("the first check nudged about the second wait: %q", n.messages())
		}
		time.Sleep(time.Minute)
		if !<-second {
			t.Fatal("the second wait was not nudged")
		}
		if got := n.messages(); len(got) != 1 || got[0] != "Agent is waiting for your answer · 2 min" {
			t.Fatalf("nudged %q", got)
		}
	})
}

// Ending a wait where there is none writes nothing, not even a lock: it runs on every prompt.
func TestEndWaitWithNoWaitWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	EndWait(filepath.Join(dir, "waiting", "s1.json"))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %v", entries)
	}
}
