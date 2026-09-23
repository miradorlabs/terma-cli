package session

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), ".git"))
}

func TestActiveSessionTTL(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	if err := s.SetActive(Session{ID: "s1", Tool: "claude-code", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if sess, fresh := s.Active(now.Add(10*time.Minute), time.Hour); !fresh || sess.ID != "s1" {
		t.Fatalf("expected fresh s1, got %+v fresh=%v", sess, fresh)
	}
	if _, fresh := s.Active(now.Add(2*time.Hour), time.Hour); fresh {
		t.Fatal("expected the session to have expired")
	}
	if err := s.ClearActive("other"); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.Active(now, 0); sess == nil {
		t.Fatal("clearing a different id must not remove the active session")
	}
	if err := s.ClearActive("s1"); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.Active(now, 0); sess != nil {
		t.Fatal("expected no active session")
	}
}

func TestRejectsUnsafeIDs(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"", "../x", "a b", ".hidden", "with/slash"} {
		if err := s.SetActive(Session{ID: id}); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if !ValidID("018f3a2c-1b2e-7c3d-9e4f-0a1b2c3d4e5f") {
		t.Fatal("uuid rejected")
	}
}

func TestTouchAndAttribute(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	claude := Session{ID: "s1", Tool: "claude-code", ToolVersion: "2.1.0"}
	codex := Session{ID: "s2", Tool: "codex", ToolVersion: "1.0"}
	if err := s.Touch(claude, []string{"./src/a.go", "src/b.go"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(codex, []string{"docs/readme.md"}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	manifests, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 2 || manifests[0].SessionID != "s1" {
		t.Fatalf("unexpected manifests: %+v", manifests)
	}

	// Staged content from both sessions plus a human-only file: two trailers.
	got := Attribute([]string{"src/a.go", "docs/readme.md", "human.txt"}, manifests, nil, false)
	if len(got) != 2 || got[0].SessionID != "s1" || got[0].Tool != "claude-code/2.1.0" || got[1].SessionID != "s2" {
		t.Fatalf("unexpected attribution: %+v", got)
	}
	if len(got[0].Files) != 1 || got[0].Files[0] != "src/a.go" {
		t.Fatalf("unexpected files: %+v", got[0].Files)
	}

	// Pure human work: nothing, even with a fresh active session? No — the
	// fallback only applies when there is no manifest evidence at all.
	active := &Session{ID: "s9", Tool: "claude-code"}
	if got := Attribute([]string{"human.txt"}, manifests, active, true); len(got) != 1 || got[0].SessionID != "s9" {
		t.Fatalf("expected the fresh active session as fallback, got %+v", got)
	}
	if got := Attribute([]string{"human.txt"}, manifests, active, false); len(got) != 0 {
		t.Fatalf("a stale active session must not claim the commit: %+v", got)
	}
	if got := Attribute(nil, manifests, active, true); len(got) != 0 {
		t.Fatalf("nothing staged means nothing attributed: %+v", got)
	}
}

func TestConsumeRemovesCommittedFiles(t *testing.T) {
	s := newStore(t)
	t0 := time.Now()
	sess := Session{ID: "s1", Tool: "claude-code"}
	if err := s.Touch(sess, []string{"a.go", "b.go"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Consume("s1", []string{"a.go"}); err != nil {
		t.Fatal(err)
	}
	manifests, _ := s.Manifests()
	if len(manifests) != 1 || len(manifests[0].Files) != 1 {
		t.Fatalf("expected b.go to remain: %+v", manifests)
	}
	if got := Attribute([]string{"a.go"}, manifests, nil, false); len(got) != 0 {
		t.Fatalf("a.go already shipped, must not attribute again: %+v", got)
	}
	if err := s.Consume("s1", []string{"b.go"}); err != nil {
		t.Fatal(err)
	}
	manifests, _ = s.Manifests()
	if len(manifests) != 1 || len(manifests[0].Files) != 0 {
		t.Fatalf("emptied manifest should be kept as evidence the session reports edits: %+v", manifests)
	}
	// With the manifest present but empty, a fresh active session must not fall
	// back onto a later human commit.
	active := &Session{ID: "s1", Tool: "claude-code"}
	if got := Attribute([]string{"human.txt"}, manifests, active, true); len(got) != 0 {
		t.Fatalf("session with a (consumed) manifest claimed human work: %+v", got)
	}
	if err := s.Consume("missing", []string{"x"}); err != nil {
		t.Fatalf("consuming an unknown session must be a no-op: %v", err)
	}
}

func TestPruneDropsOldManifests(t *testing.T) {
	s := newStore(t)
	old := time.Now().Add(-48 * time.Hour)
	if err := s.Touch(Session{ID: "old", Tool: "x"}, []string{"a"}, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(Session{ID: "new", Tool: "x"}, []string{"b"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(time.Now().Add(-24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want 1", n, err)
	}
	manifests, _ := s.Manifests()
	if len(manifests) != 1 || manifests[0].SessionID != "new" {
		t.Fatalf("unexpected survivors: %+v", manifests)
	}
}

// A subagent that edited under an id of its own is folded into the conversation that
// spawned it, so the commit is stamped once, for the session a person can find.
func TestMergeFoldsOneSessionIntoAnother(t *testing.T) {
	store := Open(t.TempDir())
	early := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	late := early.Add(time.Minute)
	parent := Session{ID: "conv-parent", Tool: "cursor"}
	child := Session{ID: "conv-child", Tool: "cursor"}

	if err := store.Touch(parent, []string{"shared.go", "parent.go"}, early); err != nil {
		t.Fatal(err)
	}
	if err := store.Touch(child, []string{"shared.go", "child.go"}, late); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(child); err != nil {
		t.Fatal(err)
	}

	moved, err := store.Merge(child.ID, parent, late)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(moved, ","); got != "child.go,shared.go" {
		t.Errorf("moved = %s", got)
	}
	manifests, err := store.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0].SessionID != parent.ID {
		t.Fatalf("manifests = %+v, want only the parent's", manifests)
	}
	files := manifests[0].Files
	if len(files) != 3 || !files["shared.go"].Equal(late) || !files["parent.go"].Equal(early) || !files["child.go"].Equal(late) {
		t.Errorf("files = %v: each keeps its own touch time, the later one where both touched it", files)
	}
	if active, _ := store.Active(late, 0); active != nil {
		t.Errorf("the folded session still claims commits through the fallback: %+v", active)
	}
}

func TestMergeEdges(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Now()
	into := Session{ID: "conv-parent", Tool: "cursor"}

	if moved, err := store.Merge("never-existed", into, now); err != nil || len(moved) != 0 {
		t.Errorf("a source with no manifest: moved=%v err=%v", moved, err)
	}
	if manifests, _ := store.Manifests(); len(manifests) != 0 {
		t.Errorf("merging nothing created a manifest: %+v", manifests)
	}
	if moved, err := store.Merge(into.ID, into, now); err != nil || len(moved) != 0 {
		t.Errorf("a session merged into itself: moved=%v err=%v", moved, err)
	}
	if _, err := store.Merge("conv-child", Session{ID: "../escape"}, now); err == nil {
		t.Error("an unsafe target id must be refused")
	}
	if moved, err := store.Merge("../escape", into, now); err != nil || len(moved) != 0 {
		t.Errorf("an unsafe source id is nothing to merge: moved=%v err=%v", moved, err)
	}
	// A target that does not exist yet takes the source's tool.
	if err := store.Touch(Session{ID: "conv-child", Tool: "cursor"}, []string{"a.go"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Merge("conv-child", Session{ID: "conv-new"}, now); err != nil {
		t.Fatal(err)
	}
	manifests, _ := store.Manifests()
	if len(manifests) != 1 || manifests[0].SessionID != "conv-new" || manifests[0].Tool != "cursor" {
		t.Errorf("manifests = %+v", manifests)
	}
}
