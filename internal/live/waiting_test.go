package live

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// agentPID is the agent process the tests' waits watch.
const agentPID = 4242

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

// procs is the agent's processes as a test sets them: whether it has gone, what it runs, and
// whether they can be listed at all.
type procs struct {
	mu       sync.Mutex
	gone     bool
	children []int
	unlisted bool
}

func (p *procs) Alive(int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.gone
}

func (p *procs) Children(int) ([]int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.children), !p.unlisted
}

func (p *procs) set(change func(*procs)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p)
}

// await runs AwaitAnswer for wait id, its result on the channel.
func await(t *testing.T, path, id string, n Notifier, p Processes) <-chan bool {
	done := make(chan bool, 1)
	go func() { done <- AwaitAnswer(t.Context(), path, id, n, p) }()
	return done
}

// watched starts wait id at path, now, on a transcript of some lines and an agent running
// two processes of its own, and returns the transcript and the agent's processes.
func watched(t *testing.T, path, id string) (transcript string, p *procs) {
	t.Helper()
	transcript = filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p = &procs{children: []int{10, 11}}
	w := Wait{ID: id, Since: time.Now(), Agent: "Agent", Checkout: "shop", Transcript: transcript, TranscriptSize: 6}
	w.Watch(agentPID, p)
	if err := StartWait(path, w); err != nil {
		t.Fatal(err)
	}
	return transcript, p
}

// A wait still going on two minutes after it began, its transcript as it was and its agent
// running nothing new, nudges the developer, once.
func TestAwaitAnswerNudgesOnceAtTwoMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "waiting", "s1.json")
		n := &told{}
		_, p := watched(t, path, "w1")
		done := await(t, path, "w1", n, p)
		time.Sleep(AnswerWait - time.Second)
		synctest.Wait()
		if got := n.messages(); len(got) != 0 {
			t.Fatalf("nudged before two minutes: %q", got)
		}
		// A process the agent ran briefly meanwhile, a status line, has gone by the check.
		p.set(func(p *procs) { p.children = []int{11} })
		time.Sleep(time.Second)
		if !<-done {
			t.Fatal("a wait of two minutes was not nudged")
		}
		if got := n.messages(); len(got) != 1 || got[0] != "Agent is waiting for your answer · shop · 2 min" {
			t.Fatalf("nudged %q", got)
		}
		if nudge(path, "w1", time.Now().Add(time.Hour), n, p) || len(n.messages()) != 1 {
			t.Fatalf("one wait nudged twice: %q", n.messages())
		}
	})
}

// Answers no hook reports end a wait as surely as those it does: a prompt rejected or
// dismissed (the agent writes it to its transcript, and ends the turn with no Stop), a tool
// approved and still running (a process the agent started since), and an agent that has gone.
func TestAwaitAnswerNeverNudgesAnAnswerNoHookReports(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(t *testing.T, transcript string, p *procs)
	}{
		{"rejected", func(t *testing.T, transcript string, _ *procs) {
			f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(`{"type":"user"}` + "\n"); err != nil {
				t.Fatal(err)
			}
		}},
		{"approved, still running", func(_ *testing.T, _ string, p *procs) {
			p.set(func(p *procs) { p.children = append(p.children, 77) })
		}},
		{"agent gone", func(_ *testing.T, _ string, p *procs) {
			p.set(func(p *procs) { p.gone = true })
		}},
	} {
		synctest.Test(t, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s1.json")
			n := &told{}
			transcript, p := watched(t, path, "w1")
			done := await(t, path, "w1", n, p)
			time.Sleep(time.Minute)
			tc.answer(t, transcript, p)
			time.Sleep(AnswerWait)
			if <-done || len(n.messages()) != 0 {
				t.Fatalf("%s: nudged %q", tc.name, n.messages())
			}
		})
	}
}

// Where the agent's processes cannot be listed, as the wait begins or at its check, the
// check nudges on the transcript and the hooks alone.
func TestAwaitAnswerNudgesWhereProcessesCannotBeListed(t *testing.T) {
	for _, when := range []string{"as the wait begins", "at the check"} {
		synctest.Test(t, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s1.json")
			n := &told{}
			p := &procs{children: []int{10}, unlisted: when == "as the wait begins"}
			w := Wait{ID: "w1", Since: time.Now(), Agent: "Agent"}
			w.Watch(agentPID, p)
			if p.unlisted && (w.AgentPID != 0 || w.Children != nil) {
				t.Fatalf("watched processes it could not list: %+v", w)
			}
			if err := StartWait(path, w); err != nil {
				t.Fatal(err)
			}
			done := await(t, path, "w1", n, p)
			time.Sleep(time.Minute)
			p.set(func(p *procs) { p.unlisted, p.children = true, []int{10, 77} })
			time.Sleep(AnswerWait)
			if !<-done || len(n.messages()) != 1 {
				t.Fatalf("%s: nudged %q, want once", when, n.messages())
			}
		})
	}
}

// A wait the developer ended in time never nudges, and leaves nothing behind.
func TestAwaitAnswerNeverNudgesAnEndedWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "waiting")
		path := filepath.Join(dir, "s1.json")
		n := &told{}
		_, p := watched(t, path, "w1")
		done := await(t, path, "w1", n, p)
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
		n, p := &told{}, &procs{}
		if err := StartWait(path, Wait{ID: "w1", Since: time.Now(), Agent: "Agent"}); err != nil {
			t.Fatal(err)
		}
		first := await(t, path, "w1", n, p)
		time.Sleep(time.Minute)
		if err := StartWait(path, Wait{ID: "w2", Since: time.Now(), Agent: "Agent"}); err != nil {
			t.Fatal(err)
		}
		second := await(t, path, "w2", n, p)
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
