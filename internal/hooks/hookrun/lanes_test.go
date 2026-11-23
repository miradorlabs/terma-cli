package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// lanedHook is a pushRepo's hook run under signIn, as auth.Account names the sign-in
// (serverKey for a server key's profile), with sandboxed hooks' failures to write the
// state directory on request: its spool and its state directory both refuse a write, so
// whatever is kept is parked. The spool refuses a write on every OS; the state directory
// only where its mode is kept (modeIgnored).
func lanedHook(t *testing.T, p *pushRepo, signIn string, serverKey, sandboxed bool) (Env, string) {
	t.Helper()
	// Reported at once: no git push is running.
	defer func(f func() int) { t.Cleanup(func() { gitPushPID = f }) }(gitPushPID)
	gitPushPID = func() int { return 0 }
	dir := t.TempDir()
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if serverKey {
		sp.BindNoPerson()
	} else {
		sp.Bind(signIn)
	}
	state := p.stateDir
	if sandboxed {
		if err := os.Mkdir(filepath.Join(dir, spool.EventsFile), 0o700); err != nil {
			t.Fatal(err)
		}
		state = t.TempDir()
		chmodReadOnly(t, state)
	}
	return Env{StateDir: state, Now: time.Now(), Cwd: p.root, Spool: sp, Team: p.team, Policy: hookruntest.Admitting(p.root),
		Account: signIn, ServerKey: serverKey, AwaitPush: func(string) {}}, dir
}

// collect runs, under env, each path that writes what a hook collects: an event, an
// observation and a push.
func collect(t *testing.T, p *pushRepo, env Env) {
	t.Helper()
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.EmitFor(r, spool.Event{Name: "terma.session.start", SessionID: "s1"})
	env.CaptureObservation(context.Background(), r, Observation{Tool: "fake", Source: "fake", StateDir: "observations", SessionID: "s1", Hook: "stop",
		Attrs: map[string]any{"turn": "t1"}})
	p.git("commit", "-q", "--allow-empty", "-m", "more")
	env.Args = []string{"backup", p.remote}
	env.Stdin = strings.NewReader(pushLine("refs/heads/main", p.sha("HEAD"), "refs/heads/main", p.sha("backup/main")) + "\n")
	if err := PrePush(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

// written lists what a hook left anywhere: queued, checkpointed, recorded or parked.
func written(t *testing.T, p *pushRepo, env Env, dir string) []string {
	t.Helper()
	var out []string
	if info, err := os.Stat(filepath.Join(dir, spool.EventsFile)); err == nil && info.Mode().IsRegular() {
		if evs := hookruntest.Spooled(t, env.Spool); len(evs) > 0 {
			out = append(out, "queued "+hookruntest.Names(evs))
		}
	}
	// An observation's lock is no record, nor is a checkpoint with nothing pending to replay.
	checkpoints, _ := filepath.Glob(filepath.Join(env.StateDir, "observations", "*.json"))
	for _, path := range checkpoints {
		var state ObservationState
		if data, err := os.ReadFile(path); err != nil || json.Unmarshal(data, &state) != nil || state.Pending != nil {
			out = append(out, path)
		}
	}
	for _, dir := range []string{filepath.Join(env.StateDir, PushesDir), filepath.Join(p.root, ".git", project.GitStoreDir)} {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			out = append(out, "wrote in "+dir)
		}
	}
	return out
}

// Signed out, or with a sign-in that can't be read, a hook collects for no lane: it writes
// nothing at all, no queued event, no checkpoint to replay, no push record and nothing
// parked, sandboxed or not, and counts each event lost by why.
func TestAHookWithNoLaneWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, signIn, why string
	}{
		{"signed out", "", sendas.LostSignedOut},
		{"unreadable", sendas.AccountUnreadable, sendas.LostUnreadable},
	} {
		for _, sandboxed := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: " sandboxed"}[sandboxed], func(t *testing.T) {
				// The count below needs a state directory that refuses the observation's sequence.
				if why := modeIgnored(); sandboxed && why != "" {
					t.Skip(why)
				}
				p := newPushRepo(t)
				env, dir := lanedHook(t, p, tc.signIn, false, sandboxed)
				collect(t, p, env)
				if w := written(t, p, env, dir); len(w) > 0 {
					t.Fatalf("a hook with no lane %v", w)
				}
				// A sandboxed hook has no state directory to keep an observation's sequence in.
				want := 3
				if sandboxed {
					want = 2
				}
				l, err := spool.ReadLosses(dir, time.Now())
				if err != nil || l.Counts[tc.why] != want || len(l.Counts) != 1 {
					t.Fatalf("losses = %v, %v; want %d %s", l.Counts, err, want, tc.why)
				}
			})
		}
	}
}

// A person's hook tags everything it writes with their account, and a server key's with no
// person, queued or parked alike.
func TestAHookTagsWhatItWritesWithItsLane(t *testing.T) {
	const person = "aaaaaaaaaaaaaaaa"
	for _, serverKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "person", true: "server key"}[serverKey], func(t *testing.T) {
			p := newPushRepo(t)
			signIn := person
			if serverKey {
				signIn = ""
			}
			env, _ := lanedHook(t, p, signIn, serverKey, false)
			collect(t, p, env)
			AwaitPush(context.Background(), env, onlyPush(t, p))
			evs := hookruntest.Spooled(t, env.Spool)
			if len(evs) != 3 {
				t.Fatalf("queued %s; want an event, an observation and a push", hookruntest.Names(evs))
			}
			for _, ev := range evs {
				if ev.Account != signIn || ev.NoPerson != serverKey {
					t.Errorf("%s queued as %q, no person %v", ev.Name, ev.Account, ev.NoPerson)
				}
			}
			// Sandboxed, what is parked carries the lane too.
			p2 := newPushRepo(t)
			env, _ = lanedHook(t, p2, signIn, serverKey, true)
			r, err := env.Repo(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !env.EmitFor(r, spool.Event{Name: "parked"}) {
				t.Fatal("nothing parked")
			}
			parked := spoolOpen(t)
			parked.Bind("bbbbbbbbbbbbbbbb")
			Env{Spool: parked, Account: "bbbbbbbbbbbbbbbb"}.unpark(r)
			if evs := hookruntest.Spooled(t, parked); len(evs) != 1 || evs[0].Account != signIn || evs[0].NoPerson != serverKey {
				t.Fatalf("unparked %+v; want it in the lane it was parked in", evs)
			}
		})
	}
}

// A sign-in switch leaves what is queued where it was collected: A's events stay A's, and
// what a hook collected signed out between A and B is never B's. Queued, checkpointed or
// parked, nothing is sent under B but B's own.
func TestASignInSwitchNeverHandsAsEventsToB(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	p := newPushRepo(t)
	queue := spoolOpen(t)
	queue.Bind(a)
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Spool: queue, Team: p.team, Policy: hookruntest.Admitting(p.root), Account: a}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.EmitFor(r, spool.Event{Name: "as-a"})
	// A signs out; a sandboxed hook runs while no one is signed in.
	out, _ := lanedHook(t, p, "", false, true)
	out.EmitFor(r, spool.Event{Name: "signed-out"})
	// B signs in, after both, and B's hook unparks and queues.
	queue.Bind(b)
	env = Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Spool: queue, Team: p.team, Policy: hookruntest.Admitting(p.root), Account: b}
	r, err = env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.EmitFor(r, spool.Event{Name: "as-b"})
	got := map[string]string{}
	for _, ev := range hookruntest.Spooled(t, queue) {
		got[ev.Name] = ev.Account
	}
	if len(got) != 2 || got["as-a"] != a || got["as-b"] != b {
		t.Fatalf("queued %v; want A's under A and only B's own under B", got)
	}
}

// onlyPush is the one push record pre-push left.
func onlyPush(t *testing.T, p *pushRepo) string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(p.stateDir, PushesDir, "*.json"))
	if len(paths) != 1 {
		t.Fatalf("push records %v", paths)
	}
	return paths[0]
}

func spoolOpen(t *testing.T) *spool.Spool {
	t.Helper()
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Signed out, the same snapshot seen again is suppressed as it is signed in: lost and
// counted once, not at every hook.
func TestASignedOutSnapshotIsCountedOnce(t *testing.T) {
	p := newPushRepo(t)
	env, dir := lanedHook(t, p, "", false, false)
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		env.CaptureObservation(context.Background(), r, Observation{Tool: "fake", Source: "fake", StateDir: "observations", SessionID: "s1", Hook: "stop",
			Attrs: map[string]any{"turn": "t1"}})
	}
	if l, _ := spool.ReadLosses(dir, time.Now()); l.Counts[sendas.LostSignedOut] != 1 {
		t.Fatalf("losses = %v; want the snapshot counted once", l.Counts)
	}
}
