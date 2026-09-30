package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexFundingTailAndIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "sessions", "2026", "09", "15", "rollout-2026-09-15-"+testCodexID+".jsonl")
	at := time.Date(2026, 9, 15, 10, 0, 0, 123000000, time.UTC)
	writeEvidenceFile(t, path, rolloutFixture(testCodexID, at, testCodexLimits)+`{"unfinished":`)
	for _, hint := range []string{path, ""} {
		e := latestCodexQuota(t, context.Background(), testCodexID, hint)
		if e.Status != "present" || !e.SourceTime.Equal(at) || e.Attrs["has_credits"] != false || e.Attrs["credits_balance"] != "12.345678901234567890" || e.Attrs["primary_used_pct"] != 0.0 {
			t.Fatalf("snapshot: %+v", e)
		}
		if _, ok := e.Attrs["secondary_used_pct"]; ok {
			t.Fatal("absent secondary converted to zero")
		}
		raw, _ := json.Marshal(e)
		for _, s := range []string{"private-repo", "never-export", "total_tokens"} {
			if strings.Contains(string(raw), s) {
				t.Fatalf("content escaped: %s", raw)
			}
		}
	}
	writeEvidenceFile(t, path, rolloutFixture("other-thread", at, testCodexLimits))
	if e := latestCodexQuota(t, context.Background(), testCodexID, path); e.Status != "session_mismatch" {
		t.Fatal(e)
	}
	if e := latestCodexQuota(t, context.Background(), testCodexID, "/tmp/foreign.jsonl"); e.Status != "unsupported_path" {
		t.Fatal(e)
	}
}

func TestCodexFundingLargeRolloutNullAndMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "archived_sessions", "rollout-2026-09-15-"+testCodexID+".jsonl")
	at := time.Now().UTC()
	original := rolloutFixture(testCodexID, at, testCodexLimits)
	_, record, _ := strings.Cut(original, "\n")
	// The reader must find metadata at the head and quota in a bounded tail.
	big := original + strings.Repeat("{\"type\":\"response_item\",\"text\":\"ignored\"}\n", 40000) + record
	writeEvidenceFile(t, path, big)
	if e := latestCodexQuota(t, context.Background(), testCodexID, path); e.Status != "present" {
		t.Fatal(e)
	}
	null := rolloutFixture(testCodexID, at.Add(time.Second), "null")
	_, nullRecord, _ := strings.Cut(null, "\n")
	writeEvidenceFile(t, path, original+nullRecord)
	if e := latestCodexQuota(t, context.Background(), testCodexID, path); e.Status != "unavailable" || len(quotaAttrs(e)) != 0 {
		t.Fatal(e)
	}
	writeEvidenceFile(t, path, rolloutFixture(testCodexID, at, `{"primary":{"used_percent":-1,"resets_at":-1},"credits":{"balance":"secret"}}`))
	e := latestCodexQuota(t, context.Background(), testCodexID, path)
	if _, ok := e.Attrs["primary_used_pct"]; ok {
		t.Fatal("bad percentage accepted")
	}
	if _, ok := e.Attrs["credits_balance"]; ok {
		t.Fatal("bad balance accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := latestCodexQuota(t, ctx, testCodexID, ""); e.Status != "search_limit" {
		t.Fatal(e)
	}
}

func TestCodexFundingRejectsSymlinksAndReportsUnreadableDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	foreign := filepath.Join(t.TempDir(), "rollout-day-"+testCodexID+".jsonl")
	writeEvidenceFile(t, foreign, rolloutFixture(testCodexID, time.Now(), testCodexLimits))
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "sessions", filepath.Base(foreign))
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	if e := latestCodexQuota(t, context.Background(), testCodexID, link); e.Status != "unsupported" || len(e.Attrs) != 0 {
		t.Fatal(e)
	}
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeEvidenceFile(t, filepath.Join(home, "sessions"), "not a directory")
	if e := latestCodexQuota(t, context.Background(), testCodexID, ""); e.Status != "unreadable" {
		t.Fatal(e)
	}
}

// latestCodexQuota reads a rollout the way the hooks do — ReadCodexFunding from an empty
// cursor, again while it reports a backlog — and returns the newest quota it emitted.
// When the rollout could not be opened at all it returns the reader's status instead,
// which is how the confinement refusals (a foreign path, a symlink, another thread's
// file) show up. These tests used to drive CodexFunding, a second tail reader nothing
// shipped called; they are the coverage of what a rollout read may touch and of what
// must never leave it, so they moved to the reader that runs.
func latestCodexQuota(t *testing.T, ctx context.Context, sessionID, transcript string) FundingEvidence {
	t.Helper()
	var last *FundingEvidence
	cursor, status := CodexCursor{}, ""
	for range 64 {
		var err error
		cursor, status, err = ReadCodexFunding(ctx, sessionID, transcript, cursor, func(e FundingEvidence) error {
			if e.Status != "gap" {
				last = &e
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read rollout: %v", err)
		}
		if status != "backlog" {
			break
		}
	}
	if last == nil {
		return FundingEvidence{Source: "codex_rollout", Status: status}
	}
	return *last
}

func TestCodexHasNoRepositoryScope(t *testing.T) {
	if ScopeOf(Codex{}) != ScopeGlobal {
		t.Error("a bare harness is global")
	}
	if _, ok := Harness(Codex{}).(Scoped); ok {
		t.Error("Codex has one config file and must not claim a repository scope")
	}
}
