package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// bound is s bound to account.
func bound(s *Store, account string) *Store {
	b := *s
	b.Bind(account)
	return &b
}

// A session's manifest names the account its first write was made under, and no write
// changes it: one under another account is refused, locked or not, and its files are not
// recorded; one that names no one claims nothing.
func TestASessionsAccountIsItsFirstWrites(t *testing.T) {
	t.Parallel()
	store := Open(t.TempDir())
	sess := Session{ID: "sess-1", Tool: "codex"}
	if err := bound(store, "aaaaaaaaaaaaaaaa").Touch(sess, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := bound(store, "bbbbbbbbbbbbbbbb").Touch(sess, []string{"b.go"}, time.Now()); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("B's touch of A's session: %v; want ErrAccountConflict", err)
	}
	// Unlocked, as while another writer holds the store.
	hasty := bound(store, "bbbbbbbbbbbbbbbb")
	hasty.lockWait = time.Millisecond
	release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	err = hasty.Touch(sess, []string{"c.go"}, time.Now())
	release()
	if !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("B's unlocked touch of A's session: %v; want ErrAccountConflict", err)
	}
	if err := store.Touch(sess, []string{"d.go"}, time.Now()); err != nil {
		t.Fatalf("a touch that names no one: %v", err)
	}
	ms, err := store.Manifests()
	if err != nil || len(ms) != 1 || ms[0].Account != "aaaaaaaaaaaaaaaa" || len(ms[0].Files) != 2 {
		t.Fatalf("manifests %+v, %v; want A's, with a.go and d.go", ms, err)
	}
}

// Deltas written unlocked as the session first changed hands never pick an owner: one
// naming another account than the stored manifest's is left out, and two with no manifest
// stored yet that disagree leave the session no one's.
func TestNoFoldPicksAnOwner(t *testing.T) {
	t.Parallel()
	for _, stored := range []bool{true, false} {
		store := Open(t.TempDir())
		sess := Session{ID: "sess-1", Tool: "codex"}
		if stored {
			if err := bound(store, "aaaaaaaaaaaaaaaa").Touch(sess, []string{"a.go"}, time.Now()); err != nil {
				t.Fatal(err)
			}
		} else if err := bound(store, "aaaaaaaaaaaaaaaa").writeDelta(newManifest(sess, "aaaaaaaaaaaaaaaa", time.Now())); err != nil {
			t.Fatal(err)
		}
		if err := store.writeDelta(newManifest(sess, "bbbbbbbbbbbbbbbb", time.Now())); err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{true: "aaaaaaaaaaaaaaaa", false: ""}[stored]
		ms, err := store.Manifests()
		if err != nil || len(ms) != 1 || ms[0].Account != want {
			t.Errorf("stored %v: manifests %+v, %v; want the session %q's", stored, ms, err, want)
		}
		if err := store.Touch(sess, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		if ms, _ := store.Manifests(); len(ms) != 1 || ms[0].Account != want {
			t.Errorf("stored %v: folded and written, %+v; want the session %q's", stored, ms, want)
		}
	}
}

// Two sessions of different accounts, or one of none, are never merged: the files stay.
func TestMergeNeverJoinsTwoOwners(t *testing.T) {
	t.Parallel()
	for _, childs := range []string{"bbbbbbbbbbbbbbbb", ""} {
		store := bound(Open(t.TempDir()), "aaaaaaaaaaaaaaaa")
		parent := Session{ID: "conv-parent", Tool: "cursor"}
		if err := store.Touch(parent, []string{"p.go"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := bound(store, childs).Touch(Session{ID: "conv-child", Tool: "cursor"}, []string{"c.go"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if moved, err := store.Merge("conv-child", parent, time.Now()); !errors.Is(err, ErrAccountConflict) || len(moved) != 0 {
			t.Errorf("merging %q's child: %v, %v; want ErrAccountConflict", childs, moved, err)
		}
		if _, err := os.Stat(store.manifestPath(Key{Tool: "cursor", ID: "conv-child"})); err != nil {
			t.Errorf("the child's manifest went: %v", err)
		}
	}
	// Merging A's child into a new parent under B is refused without claiming the parent for B.
	store := Open(t.TempDir())
	child := Session{ID: "conv-child", Tool: "cursor"}
	parent := Session{ID: "conv-parent", Tool: "cursor"}
	if err := bound(store, "aaaaaaaaaaaaaaaa").Touch(child, []string{"c.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if moved, err := bound(store, "bbbbbbbbbbbbbbbb").Merge(child.ID, parent, time.Now()); !errors.Is(err, ErrAccountConflict) || len(moved) != 0 {
		t.Fatalf("B merging A's child into new parent: %v, %v; want ErrAccountConflict", moved, err)
	}
	if fileExists(store.ownerPath(parent.Key())) {
		t.Errorf("a refused merge left an owner file for %s", parent.ID)
	}
}

// Deltas with no manifest or owner file stored yet give a session the same owner in either
// order: one that names no one neither sets it nor clears it.
func TestDeltaOrderNeverDecidesAnOwner(t *testing.T) {
	t.Parallel()
	for _, order := range [][2]string{{"", "aaaaaaaaaaaaaaaa"}, {"aaaaaaaaaaaaaaaa", ""}} {
		store := Open(t.TempDir())
		sess := Session{ID: "sess-1", Tool: "codex"}
		for i, account := range order {
			path := filepath.Join(store.dir, manifestsDir, sess.Tool, sess.ID+deltaSep+string(rune('0'+i))+deltaExt)
			if err := writeJSON(path, newManifest(sess, account, time.Now())); err != nil {
				t.Fatal(err)
			}
		}
		if ms, _ := store.Manifests(); len(ms) != 1 || ms[0].Account != "aaaaaaaaaaaaaaaa" {
			t.Errorf("deltas %q: read as %+v; want A's", order, ms)
		}
		if m, _, _ := store.load(sess.Key()); m == nil || m.Account != "aaaaaaaaaaaaaaaa" {
			t.Errorf("deltas %q: loaded as %+v; want A's", order, m)
		}
	}
}

// A session with no manifest is new, and owned by whoever writes it first; one whose
// manifest can't be read is no one's to write, since its owner can't be checked — whether
// or not an intact owner file names the caller.
func TestAnUnreadableManifestIsNotOwned(t *testing.T) {
	t.Parallel()
	store := bound(Open(t.TempDir()), "bbbbbbbbbbbbbbbb")
	sess := Session{ID: "sess-1", Tool: "codex"}
	if !store.Owns(sess.Key()) {
		t.Fatal("a session with no manifest is not owned")
	}
	if err := store.Touch(sess, []string{"b.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.manifestPath(sess.Key()), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if store.Owns(sess.Key()) {
		t.Fatal("a session whose manifest can't be read is owned (with owner file)")
	}
	if err := os.Remove(store.ownerPath(sess.Key())); err != nil {
		t.Fatal(err)
	}
	if store.Owns(sess.Key()) {
		t.Fatal("a session whose manifest can't be read is owned (without owner file)")
	}
}

// A session whose manifest can't be read is no one's to write or to name: a write under
// the lock or waiting on it is refused, Accounts names it unreadable, and Manifests leaves
// it out with its deltas, which would name their own account in its place.
func TestAnUnreadableManifestNamesNoOne(t *testing.T) {
	t.Parallel()
	store := Open(t.TempDir())
	sess := Session{ID: "sess-1", Tool: "codex"}
	a, b := bound(store, "aaaaaaaaaaaaaaaa"), bound(store, "bbbbbbbbbbbbbbbb")
	if err := a.Touch(sess, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A delta B wrote while the manifest was still A's, and whole.
	if err := b.writeDelta(newManifest(sess, "bbbbbbbbbbbbbbbb", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.manifestPath(sess.Key()), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Touch(sess, []string{"b.go"}, time.Now()); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("B's touch of an unreadable manifest: %v; want ErrAccountConflict", err)
	}
	hasty := bound(store, "bbbbbbbbbbbbbbbb")
	hasty.lockWait = time.Millisecond
	release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	err = hasty.Touch(sess, []string{"c.go"}, time.Now())
	release()
	if !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("B's unlocked touch of an unreadable manifest: %v; want ErrAccountConflict", err)
	}
	if got := store.Accounts([]Key{sess.Key(), {Tool: "codex", ID: "sess-new"}, {ID: "no-tool"}}); !slices.Equal(got, []string{sendas.AccountUnreadable, "", ""}) {
		t.Fatalf("accounts = %q; want the unreadable one marked, the missing ones none", got)
	}
	if ms, err := store.Manifests(); err != nil || len(ms) != 0 {
		t.Fatalf("manifests %+v, %v; want the unreadable session left out", ms, err)
	}
}

// A torn manifest, left out of Manifests, still ages out with its deltas, by its file's
// time: one past retention is pruned and its store can be retired; a fresh one is kept.
func TestATornManifestIsPrunedAtRetention(t *testing.T) {
	t.Parallel()
	torn := func(store *Store, id string, at time.Time) Key {
		t.Helper()
		sess := Session{ID: id, Tool: "codex"}
		if err := bound(store, "aaaaaaaaaaaaaaaa").Touch(sess, []string{"a.go"}, at); err != nil {
			t.Fatal(err)
		}
		if err := store.writeDelta(newManifest(sess, "bbbbbbbbbbbbbbbb", at)); err != nil {
			t.Fatal(err)
		}
		path := store.manifestPath(sess.Key())
		if err := os.WriteFile(path, []byte("{torn"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return sess.Key()
	}
	files := func(store *Store, k Key) []string {
		paths, _ := filepath.Glob(filepath.Join(store.dir, manifestsDir, k.Tool, k.ID+"*"))
		return paths
	}
	now := time.Now()
	store := Open(t.TempDir())
	old, fresh := torn(store, "sess-old", now.Add(-48*time.Hour)), torn(store, "sess-fresh", now)
	if n, err := store.Prune(now.Add(-24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want the old torn session", n, err)
	}
	if got := files(store, old); len(got) != 0 {
		t.Errorf("the old torn session left %v", got)
	}
	if got := files(store, fresh); len(got) != 3 {
		t.Errorf("the fresh torn session has %v; want its manifest, its owner and its delta kept", got)
	}
	retired := Open(t.TempDir())
	torn(retired, "sess-old", now.Add(-48*time.Hour))
	if err := os.Chtimes(filepath.Join(retired.dir, lockFile), now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	retired.Retire(now.Add(-24 * time.Hour))
	if !retired.missing() {
		entries, _ := os.ReadDir(retired.dir)
		t.Fatalf("a store holding only an aged torn session was not retired: %v", entries)
	}
}
