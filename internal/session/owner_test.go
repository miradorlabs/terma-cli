package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// Concurrent claims on a new session race atomically on every platform (including Windows):
// exactly one account wins and every caller observes that same winner.
func TestConcurrentClaimsPickOneOwner(t *testing.T) {
	t.Parallel()
	const a, b = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	for round := range 10 {
		dir := t.TempDir()
		k := Key{Tool: "codex", ID: "sess-race"}
		const writers = 12
		results := make([]string, writers)
		var wg sync.WaitGroup
		for i := range writers {
			account := []string{a, b}[i%2]
			store := bound(Open(dir), account)
			wg.Go(func() {
				owner, err := store.claim(k)
				if err != nil {
					t.Errorf("round %d writer %d: %v", round, i, err)
					return
				}
				results[i] = owner
			})
		}
		wg.Wait()
		winner := results[0]
		if winner != a && winner != b {
			t.Fatalf("round %d: unexpected winner %q", round, winner)
		}
		for i, got := range results {
			if got != winner {
				t.Fatalf("round %d: writer %d saw %q; want %q", round, i, got, winner)
			}
		}
	}
}

// Accounts reports AccountUnreadable for a session whose owner file cannot be read.
func TestAccountsReportsUnreadableOwner(t *testing.T) {
	t.Parallel()
	store := bound(Open(t.TempDir()), "aaaaaaaaaaaaaaaa")
	sess := Session{ID: "sess-1", Tool: "codex"}
	if err := store.Touch(sess, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.ownerPath(sess.Key()), []byte("{torn"), fileMode); err != nil {
		t.Fatal(err)
	}
	if got := store.Accounts([]Key{sess.Key()}); !slices.Equal(got, []string{sendas.AccountUnreadable}) {
		t.Fatalf("accounts = %q; want [%s]", got, sendas.AccountUnreadable)
	}
}

// writeExclusive (the O_CREATE|O_EXCL fallback when os.Link is unsupported) writes the first
// caller's content and fails with fs.ErrExist for a second caller without replacing it.
func TestWriteExclusiveIsFirstWriterWins(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sess.owner")
	if err := writeExclusive(path, []byte(`"aaaaaaaaaaaaaaaa"`)); err != nil {
		t.Fatalf("first writeExclusive: %v", err)
	}
	if err := writeExclusive(path, []byte(`"bbbbbbbbbbbbbbbb"`)); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second writeExclusive: %v; want fs.ErrExist", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `"aaaaaaaaaaaaaaaa"` {
		t.Fatalf("owner file = %q, %v; want A's claim untouched", got, err)
	}
}

// A foreign delta planted beside an owner file (with no manifest yet) is ignored by both
// Manifests and Accounts, and a matching delta keeps the owner's account.
func TestOwnerFiltersForeignDeltaWithNoManifest(t *testing.T) {
	t.Parallel()
	store := Open(t.TempDir())
	sess := Session{ID: "sess-foreign", Tool: "codex"}
	if err := store.link(sess.Key(), "aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDelta(newManifest(sess, "bbbbbbbbbbbbbbbb", time.Now())); err != nil {
		t.Fatal(err)
	}
	if ms, err := store.Manifests(); err != nil || len(ms) != 0 {
		t.Fatalf("manifests with foreign delta alone = %+v, %v; want none", ms, err)
	}
	if got := store.Accounts([]Key{sess.Key()}); !slices.Equal(got, []string{""}) {
		t.Fatalf("accounts with foreign delta alone = %q; want [\"\"]", got)
	}
	aDelta := newManifest(sess, "aaaaaaaaaaaaaaaa", time.Now())
	aDelta.Files["a.go"] = time.Now()
	if err := store.writeDelta(aDelta); err != nil {
		t.Fatal(err)
	}
	ms, err := store.Manifests()
	if err != nil || len(ms) != 1 || ms[0].Account != "aaaaaaaaaaaaaaaa" || len(ms[0].Files) != 1 {
		t.Fatalf("manifests with A's and foreign delta = %+v, %v; want A's file alone", ms, err)
	}
	if got := store.Accounts([]Key{sess.Key()}); !slices.Equal(got, []string{"aaaaaaaaaaaaaaaa"}) {
		t.Fatalf("accounts = %q; want A", got)
	}
}

// Merge into a target that already has an owner file (but no manifest yet) succeeds when the
// owner matches the source's account and returns ErrAccountConflict when it does not.
func TestMergeRespectsOwnerOnlyTarget(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := Open(t.TempDir())
	childA := Session{ID: "conv-child-a", Tool: "cursor"}
	if err := bound(store, "aaaaaaaaaaaaaaaa").Touch(childA, []string{"a.go"}, now); err != nil {
		t.Fatal(err)
	}
	// Target already claimed by B: merging A's child is refused.
	parentB := Session{ID: "conv-parent-b", Tool: "cursor"}
	if err := store.link(parentB.Key(), "bbbbbbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	if moved, err := bound(store, "aaaaaaaaaaaaaaaa").Merge(childA.ID, parentB, now); !errors.Is(err, ErrAccountConflict) || len(moved) != 0 {
		t.Fatalf("merging A into B-claimed target: %v, %v; want ErrAccountConflict", moved, err)
	}
	// Target already claimed by A: merging A's child succeeds and moves the file.
	parentA := Session{ID: "conv-parent-a", Tool: "cursor"}
	if err := store.link(parentA.Key(), "aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if moved, err := bound(store, "aaaaaaaaaaaaaaaa").Merge(childA.ID, parentA, now); err != nil || !slices.Equal(moved, []string{"a.go"}) {
		t.Fatalf("merging A into A-claimed target: %v, %v; want [a.go]", moved, err)
	}
}

// claim retries briefly when an owner file is empty (as during the O_CREATE|O_EXCL fallback),
// succeeding once it fills and failing closed with ErrAccountConflict if it stays empty.
func TestClaimRetriesTransientEmptyOwner(t *testing.T) {
	t.Parallel()
	store := bound(Open(t.TempDir()), "aaaaaaaaaaaaaaaa")
	store.emptyOwnerWait = func() {}
	k := Key{Tool: "codex", ID: "sess-empty"}
	ownerPath := store.ownerPath(k)
	if err := os.MkdirAll(filepath.Dir(ownerPath), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerPath, nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.claim(k); !errors.Is(err, ErrAccountConflict) {
		t.Fatalf("claim on permanently empty owner: %v; want ErrAccountConflict", err)
	}
	retries := 0
	store.emptyOwnerWait = func() {
		retries++
		if retries == 2 {
			_ = os.WriteFile(ownerPath, []byte(`"bbbbbbbbbbbbbbbb"`), fileMode)
		}
	}
	got, err := store.claim(k)
	if err != nil || got != "bbbbbbbbbbbbbbbb" || retries != 2 {
		t.Fatalf("claim after empty owner filled = %q, %v (retries=%d); want B after 2 retries", got, err, retries)
	}
}

// A torn owner file beside deltas (with no manifest) ages out by the owner file's time.
func TestPruneDropsTornOwnerWithDeltaAtRetention(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := Open(t.TempDir())
	sess := Session{ID: "sess-torn-owner", Tool: "codex"}
	if err := store.writeDelta(newManifest(sess, "aaaaaaaaaaaaaaaa", now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	ownerPath := store.ownerPath(sess.Key())
	if err := os.WriteFile(ownerPath, []byte("{torn"), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(ownerPath, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := store.Prune(now.Add(-24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want 1 torn owner-only session", n, err)
	}
	if fileExists(ownerPath) || store.hasDeltas(sess.Key()) {
		t.Fatalf("aged torn owner or its delta remained after prune")
	}
}

// An aged owner file is kept while its session still has a live delta (with no manifest yet)
// or a live manifest; only a lone aged owner with neither is removed.
func TestPruneKeepsAgedOwnerWithLiveDeltaOrManifest(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	store := bound(Open(t.TempDir()), "aaaaaaaaaaaaaaaa")

	deltaOnly := Session{ID: "sess-delta-only", Tool: "codex"}
	if err := store.link(deltaOnly.Key(), "aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(store.ownerPath(deltaOnly.Key()), old, old); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDelta(newManifest(deltaOnly, "aaaaaaaaaaaaaaaa", now)); err != nil {
		t.Fatal(err)
	}

	manifestOnly := Session{ID: "sess-manifest-only", Tool: "codex"}
	if err := store.Touch(manifestOnly, []string{"a.go"}, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(store.ownerPath(manifestOnly.Key()), old, old); err != nil {
		t.Fatal(err)
	}

	lone := Session{ID: "sess-lone-old", Tool: "codex"}
	if err := store.link(lone.Key(), "aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(store.ownerPath(lone.Key()), old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Prune(now.Add(-24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !fileExists(store.ownerPath(deltaOnly.Key())) {
		t.Fatalf("aged owner with live delta was pruned")
	}
	if !fileExists(store.ownerPath(manifestOnly.Key())) {
		t.Fatalf("aged owner with live manifest was pruned")
	}
	if fileExists(store.ownerPath(lone.Key())) {
		t.Fatalf("aged lone owner survived prune")
	}
}
