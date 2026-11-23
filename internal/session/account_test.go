package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
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
}

// Deltas with no manifest stored yet give a session the same owner in either order: one that
// names no one neither sets it nor clears it.
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
