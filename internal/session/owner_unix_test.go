//go:build unix

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// held takes store's lock for the test, so every writer misses it and writes a delta.
func held(t *testing.T, store *Store) {
	t.Helper()
	if err := os.MkdirAll(store.dir, dirMode); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

// A session first written as a delta, while another writer held the store, is its writer's:
// a later write under another account that misses the lock too is refused, not recorded as a
// delta of its own (PR #152, comment 7101173452).
func TestADeltaClaimsTheSession(t *testing.T) {
	t.Parallel()
	store := Open(t.TempDir())
	store.lockWait = time.Millisecond
	held(t, store)
	sess := Session{ID: "sess-1", Tool: "codex"}
	if err := bound(store, "aaaaaaaaaaaaaaaa").Touch(sess, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := bound(store, "bbbbbbbbbbbbbbbb").Touch(sess, []string{"b.go"}, time.Now()); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("B's unlocked touch of A's delta-only session: %v; want ErrAccountConflict", err)
	}
	ms, err := store.Manifests()
	if err != nil || len(ms) != 1 || ms[0].Account != "aaaaaaaaaaaaaaaa" || len(ms[0].Files) != 1 {
		t.Fatalf("manifests %+v, %v; want A's session, with a.go alone", ms, err)
	}
}

// Two accounts' writers racing to write a new session, some under the lock and some missing
// it, leave it one account's: every write of that account is recorded, every write of the
// other is refused, and none of the loser's files is in the manifest.
func TestRacingAccountsLeaveOneOwner(t *testing.T) {
	t.Parallel()
	const a, b = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	for round := range 20 {
		dir := t.TempDir()
		sess := Session{ID: "sess-1", Tool: "codex"}
		const writers = 16
		var mu sync.Mutex
		ok := map[string]int{}
		var wg sync.WaitGroup
		for i := range writers {
			account := []string{a, b}[i%2]
			store := bound(Open(dir), account)
			store.lockWait = []time.Duration{time.Minute, time.Microsecond}[i/2%2]
			wg.Go(func() {
				err := store.Touch(sess, []string{fmt.Sprintf("%s-%02d.go", account[:1], i)}, time.Now())
				if err != nil && !errors.Is(err, ErrAccountConflict) {
					t.Errorf("touch %d: %v", i, err)
				}
				if err == nil {
					mu.Lock()
					ok[account]++
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if len(ok) != 1 {
			t.Fatalf("round %d: writes recorded per account %v; want one account's alone", round, ok)
		}
		winner := a
		if ok[b] > 0 {
			winner = b
		}
		if ok[winner] != writers/2 {
			t.Fatalf("round %d: %d of the winner's %d writes recorded", round, ok[winner], writers/2)
		}
		ms, err := patient(Open(dir)).Manifests()
		if err != nil || len(ms) != 1 || ms[0].Account != winner || len(ms[0].Files) != writers/2 {
			t.Fatalf("round %d: manifests %+v, %v; want %s's, with its %d files alone", round, ms, err, winner, writers/2)
		}
		for f := range ms[0].Files {
			if f[:1] != winner[:1] {
				t.Fatalf("round %d: the loser's %s is in the winner's manifest", round, f)
			}
		}
	}
}

// An owner file that can't be read names no one can check: every write is refused, locked or
// not, and the session is not owned. One missing beside a manifest is claimed for the account
// the manifest names, never the writer's.
func TestAnOwnerIsReadOrTheWriteFails(t *testing.T) {
	t.Parallel()
	sess := Session{ID: "sess-1", Tool: "codex"}
	store := bound(Open(t.TempDir()), "aaaaaaaaaaaaaaaa")
	if err := store.Touch(sess, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{torn", "null"} {
		if err := os.WriteFile(store.ownerPath(sess.Key()), []byte(bad), fileMode); err != nil {
			t.Fatal(err)
		}
		if store.Owns(sess.Key()) {
			t.Errorf("owner %q: a session whose owner can't be read is owned", bad)
		}
		if err := store.Touch(sess, []string{"b.go"}, time.Now()); !errors.Is(err, ErrAccountConflict) {
			t.Errorf("owner %q: a locked touch of an unreadable owner: %v; want ErrAccountConflict", bad, err)
		}
	}
	hasty := bound(store, "aaaaaaaaaaaaaaaa")
	hasty.lockWait = time.Millisecond
	func() {
		release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if err := hasty.Touch(sess, []string{"c.go"}, time.Now()); !errors.Is(err, ErrAccountConflict) {
			t.Errorf("an unlocked touch of an unreadable owner: %v; want ErrAccountConflict", err)
		}
	}()
	if ms, err := store.Manifests(); err != nil || len(ms) != 0 {
		t.Errorf("manifests %+v, %v; want the session whose owner can't be read left out", ms, err)
	}
	if err := os.Remove(store.ownerPath(sess.Key())); err != nil {
		t.Fatal(err)
	}
	if err := bound(store, "bbbbbbbbbbbbbbbb").Touch(sess, []string{"b.go"}, time.Now()); !errors.Is(err, ErrAccountConflict) {
		t.Errorf("B's touch of A's manifest with no owner file: %v; want ErrAccountConflict", err)
	}
	if o, err := store.owner(sess.Key()); err != nil || !o.claimed || o.account != "aaaaaaaaaaaaaaaa" {
		t.Errorf("owner %+v, %v; want it claimed for the manifest's A", o, err)
	}
	// Same under the lock-missed path: B's unlocked touch of A's manifest with no owner file
	// claims it for A and refuses B.
	if err := os.Remove(store.ownerPath(sess.Key())); err != nil {
		t.Fatal(err)
	}
	func() {
		release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		hastyB := bound(store, "bbbbbbbbbbbbbbbb")
		hastyB.lockWait = time.Millisecond
		if err := hastyB.Touch(sess, []string{"b2.go"}, time.Now()); !errors.Is(err, ErrAccountConflict) {
			t.Errorf("B's unlocked touch of A's manifest with no owner file: %v; want ErrAccountConflict", err)
		}
	}()
	if o, err := store.owner(sess.Key()); err != nil || !o.claimed || o.account != "aaaaaaaaaaaaaaaa" {
		t.Errorf("owner after unlocked touch %+v, %v; want it claimed for the manifest's A", o, err)
	}
}

// An unlocked first Touch that names no one claims "" for good, just like a locked first
// Touch: a later write under A keeps the session naming no one.
func TestAnUnlockedUnnamedFirstTouchClaimsNoOneForGood(t *testing.T) {
	t.Parallel()
	store := Open(t.TempDir())
	store.lockWait = time.Millisecond
	held(t, store)
	sess := Session{ID: "sess-unnamed", Tool: "codex"}
	if err := store.Touch(sess, []string{"old.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := bound(store, "aaaaaaaaaaaaaaaa").Touch(sess, []string{"new.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ms, err := store.Manifests()
	if err != nil || len(ms) != 1 || ms[0].Account != "" || len(ms[0].Files) != 2 {
		t.Fatalf("manifests %+v, %v; want unnamed session with both files", ms, err)
	}
}

// An owner left with nothing else of its session, by a writer killed after its claim, is
// pruned at retention; a fresh one may be a writer's about to record, and stays — keeping
// the store from being retired until the owner ages out.
func TestALoneOwnerIsPrunedAtRetention(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := Open(t.TempDir())
	old, fresh := Key{Tool: "codex", ID: "sess-old"}, Key{Tool: "codex", ID: "sess-fresh"}
	for _, k := range []Key{old, fresh} {
		if err := store.link(k, "aaaaaaaaaaaaaaaa"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(store.ownerPath(old), now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(now.Add(-24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fileExists(store.ownerPath(old)) || !fileExists(store.ownerPath(fresh)) {
		t.Errorf("old owner kept %v, fresh kept %v; want only the fresh one", fileExists(store.ownerPath(old)), fileExists(store.ownerPath(fresh)))
	}
	// Retire keeps a store holding only a fresh lone owner, and removes it once aged out.
	retired := Open(t.TempDir())
	if err := os.MkdirAll(retired.dir, dirMode); err != nil {
		t.Fatal(err)
	}
	if err := retired.link(fresh, "aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired.dir, lockFile), nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(retired.dir, lockFile), now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	retired.Retire(now.Add(-24 * time.Hour))
	if retired.missing() || !fileExists(retired.ownerPath(fresh)) {
		t.Fatalf("Retire removed a store holding a fresh lone owner")
	}
	if err := os.Chtimes(retired.ownerPath(fresh), now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	retired.Retire(now.Add(-24 * time.Hour))
	if !retired.missing() {
		t.Fatalf("Retire kept a store holding only an aged lone owner")
	}
}

// Failing to create a brand-new owner file in a read-only store directory returns an I/O
// error rather than ErrAccountConflict, so unwritable stores are not miscounted as conflicts.
func TestReadOnlyStoreDoesNotReportAccountConflict(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores chmod 0o500")
	}
	t.Parallel()
	store := bound(Open(t.TempDir()), "aaaaaaaaaaaaaaaa")
	sess := Session{ID: "sess-ro", Tool: "codex"}
	toolDir := filepath.Dir(store.ownerPath(sess.Key()))
	if err := os.MkdirAll(toolDir, dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(toolDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(toolDir, dirMode) })
	err := store.Touch(sess, []string{"a.go"}, time.Now())
	if err == nil {
		t.Fatal("touch in read-only tool dir succeeded; want error")
	}
	if errors.Is(err, ErrAccountConflict) {
		t.Fatalf("touch in read-only tool dir returned ErrAccountConflict (%v); want raw I/O error", err)
	}
}
