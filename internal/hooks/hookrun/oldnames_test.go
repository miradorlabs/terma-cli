package hookrun

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Where a terma before user tokens (v0.3.x) records pushes under the state directory, and
// parks events and pushes in .git/terma.
const (
	olderPushesDir     = "pushes"
	olderParkedEvents  = "events"
	olderParkedPushes  = "pushes"
	olderParkedFileExt = ".jsonl"
)

// What a hook records or parks lands only under this version's names, never an older
// terma's: a push recorded in the state directory, and events and pushes a sandboxed hook
// parks in .git/terma.
func TestRecordsAndParksLandOnlyUnderThisVersionsNames(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	gitPushPID = func() int { return 0 }
	p := newPushRepo(t)
	if err := gitStore(p, personA).Touch(session.Session{ID: "sess-a", Tool: "claude-code"}, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	sandboxed := sandboxedHook(t, p)
	prePush(t, p, sandboxed, commit(t, p, sandboxed, []string{"a.go"}, "sess-a"))
	gitTerma := filepath.Join(p.root, ".git", project.GitStoreDir)
	for _, name := range []string{parkedEvents, parkedPushes} {
		if _, err := os.Stat(filepath.Join(gitTerma, name+olderParkedFileExt)); err != nil {
			t.Errorf("nothing parked under %s: %v", name, err)
		}
	}
	for _, name := range []string{olderParkedEvents, olderParkedPushes} {
		if _, err := os.Stat(filepath.Join(gitTerma, name+olderParkedFileExt)); err == nil {
			t.Errorf("parked under an older terma's %s", name)
		}
	}
	outside, _ := signedIn(t, p, personA)
	outside.AwaitPush = func(string) {}
	prePush(t, p, outside, p.sha("HEAD"))
	// The sandboxed push, unparked as pre-push opens the repository, is recorded there too.
	if entries, _ := os.ReadDir(filepath.Join(p.stateDir, PushesDir)); len(entries) != 2 {
		t.Errorf("%d push records under %s, want both pushes", len(entries), PushesDir)
	}
	if _, err := os.Stat(filepath.Join(p.stateDir, olderPushesDir)); err == nil {
		t.Errorf("a push was recorded under an older terma's %s", olderPushesDir)
	}
}

// What an older terma recorded or parked under its own names is never read: a push record
// in its state directory's pushes is not swept, nor one parked in .git/terma's pushes
// recorded, nor a parked event queued, even when each names the sign-in in place.
func TestAnOlderTermasRecordsAndParksAreNeverRead(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	gitPushPID = func() int { return 0 }
	p := newPushRepo(t)
	if err := gitStore(p, personA).Touch(session.Session{ID: "sess-a", Tool: "claude-code"}, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A whole push record, tagged as this version writes it, moved under the older names.
	recorder, _ := signedIn(t, p, personA)
	recorder.AwaitPush = func(string) {}
	prePush(t, p, recorder, commit(t, p, recorder, []string{"a.go"}, "sess-a"))
	records, _ := filepath.Glob(filepath.Join(p.stateDir, PushesDir, "*.json"))
	if len(records) != 1 {
		t.Fatalf("push records %v, want one", records)
	}
	rec, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(p.stateDir, PushesDir)); err != nil {
		t.Fatal(err)
	}
	olderRecord := filepath.Join(p.stateDir, olderPushesDir, filepath.Base(records[0]))
	if err := os.MkdirAll(filepath.Dir(olderRecord), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(olderRecord, rec, 0o600); err != nil {
		t.Fatal(err)
	}
	abandoned := time.Now().Add(-2 * pushAbandoned)
	if err := os.Chtimes(olderRecord, abandoned, abandoned); err != nil {
		t.Fatal(err)
	}
	parking := session.Open(filepath.Join(p.root, ".git", project.GitStoreDir))
	event := []byte(`{"name":"older","time":"2026-11-20T00:00:00Z","account":"` + personA + `","attrs":{"terma.project.id":"t"}}` + "\n")
	if err := parking.Park(olderParkedEvents, event); err != nil {
		t.Fatal(err)
	}
	var line bytes.Buffer
	if err := json.Compact(&line, rec); err != nil {
		t.Fatal(err)
	}
	if err := parking.Park(olderParkedPushes, append(line.Bytes(), '\n')); err != nil {
		t.Fatal(err)
	}

	outside, _ := signedIn(t, p, personA)
	var awaited []string
	outside.AwaitPush = func(path string) { awaited = append(awaited, path) }
	SweepPushes(context.Background(), outside)
	if _, err := outside.Repo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lanes(t, outside.Spool); len(got) != 0 {
		t.Errorf("queued %v from an older terma's names", got)
	}
	if len(awaited) != 0 {
		t.Errorf("awaited %v from an older terma's parked pushes", awaited)
	}
	if _, err := os.Stat(olderRecord); err != nil {
		t.Errorf("the older push record was taken: %v", err)
	}
}
