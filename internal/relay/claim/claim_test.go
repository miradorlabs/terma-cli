package claim

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func enable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirName, tokenFile), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWriteReadRefresh(t *testing.T) {
	enable(t)
	if !Enabled() {
		t.Fatal("a token means the relay is set up")
	}
	now := time.Now()
	if !Write("sess-1", Claim{ProjectID: "p1", Tool: "codex"}, now) {
		t.Fatal("first claim not written")
	}
	if Write("sess-1", Claim{ProjectID: "p1"}, now.Add(time.Minute)) {
		t.Fatal("a fresh claim for the same project is not rewritten")
	}
	if !Write("sess-1", Claim{ProjectID: "p2"}, now.Add(time.Minute)) {
		t.Fatal("a claim for another project is rewritten at once")
	}
	c, ok := Read("sess-1", now.Add(time.Minute))
	if !ok || c.ProjectID != "p2" {
		t.Fatalf("read = %+v, %v", c, ok)
	}
}

func TestReadRejectsStaleAndUnsafe(t *testing.T) {
	dir := enable(t)
	now := time.Now()
	Write("sess-1", Claim{ProjectID: "p1"}, now)
	p := filepath.Join(dir, DirName, claimsDir, "sess-1.json")
	old := now.Add(-TTL - time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := Read("sess-1", now); ok {
		t.Fatal("a claim past TTL is not live")
	}
	Prune(now)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("prune keeps a stale claim")
	}
	for _, bad := range []string{"", "../x", ".hidden", "a/b"} {
		if Write(bad, Claim{ProjectID: "p1"}, now) {
			t.Fatalf("unsafe session id %q was written", bad)
		}
	}
	if Write("sess-2", Claim{}, now) {
		t.Fatal("a claim without a project is not written")
	}
}

// Each hook of a session adds the processes it ran under; a hook whose processes are
// already named, inside Refresh, writes nothing.
func TestClaimMergesProcesses(t *testing.T) {
	enable(t)
	now := time.Now()
	Write("s", Claim{ProjectID: "p", PIDs: []int{10, 11}}, now)
	if Write("s", Claim{ProjectID: "p", PIDs: []int{11}}, now) {
		t.Fatal("known processes rewrote a fresh claim")
	}
	if !Write("s", Claim{ProjectID: "p", PIDs: []int{20, 21}}, now) {
		t.Fatal("a new run of the session did not add its processes")
	}
	c, _ := Read("s", now)
	if !c.Covers(10) || !c.Covers(21) || c.Covers(99) {
		t.Fatalf("claim %v", c.PIDs)
	}
	if !(Claim{}).Covers(99) || !c.Covers(0) {
		t.Fatal("a claim without processes, or a sender that could not be resolved, is covered")
	}
}
