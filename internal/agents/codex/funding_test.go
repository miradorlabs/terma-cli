package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func TestCodexFundingTailAndIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "sessions", "2026", "09", "15", "rollout-2026-09-15-"+testCodexID+".jsonl")
	at := time.Date(2026, 9, 15, 10, 0, 0, 123000000, time.UTC)
	writeEvidenceFile(t, path, rolloutFixture(testCodexID, at, testCodexLimits)+`{"unfinished":`)
	for _, hint := range []string{path, ""} {
		e := latestCodexQuota(t, context.Background(), testCodexID, hint)
		if e.Status != "present" || !e.SourceTime.Equal(at) || e.Attrs[semconv.TermaCreditsPresentKey] != false || e.Attrs[semconv.TermaCreditsBalanceKey] != "12.345678901234567890" || e.Attrs[semconv.TermaRateLimitPrimaryUsedPercentKey] != 0.0 {
			t.Fatalf("snapshot: %+v", e)
		}
		if _, ok := e.Attrs[semconv.TermaRateLimitSecondaryUsedPercentKey]; ok {
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
	if _, ok := e.Attrs[semconv.TermaRateLimitPrimaryUsedPercentKey]; ok {
		t.Fatal("bad percentage accepted")
	}
	if _, ok := e.Attrs[semconv.TermaCreditsBalanceKey]; ok {
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

// latestCodexQuota reads a rollout as the hooks do and returns the newest quota, or the
// reader's status when the open was refused.
func latestCodexQuota(t *testing.T, ctx context.Context, sessionID, transcript string) hookrun.FundingEvidence {
	t.Helper()
	var last *hookrun.FundingEvidence
	cursor, status := quotaCursor{}, ""
	for range 64 {
		var err error
		cursor, status, err = readFunding(ctx, sessionID, transcript, cursor, admitAll, func(e hookrun.FundingEvidence) error {
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
		return hookrun.FundingEvidence{Source: "codex_rollout", Status: status}
	}
	return *last
}

func TestCodexHasNoRepositoryScope(t *testing.T) {
	t.Parallel()
	if _, ok := (exporter{}).Local(t.TempDir()); ok {
		t.Error("Codex has one config file and must not claim a repository scope")
	}
}
