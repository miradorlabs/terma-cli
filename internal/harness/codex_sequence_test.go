package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sequenceFixture(t *testing.T, records string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "sessions", "rollout-day-"+testCodexID+".jsonl")
	writeEvidenceFile(t, path, fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", testCodexID)+records)
	return path
}
func quotaRecord(limits string) string {
	_, record, _ := strings.Cut(rolloutFixture(testCodexID, time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC), limits), "\n")
	return record
}
func appendRollout(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}
func readSequence(t *testing.T, path string, c CodexCursor) (CodexCursor, string, []FundingEvidence) {
	t.Helper()
	var evs []FundingEvidence
	next, status, err := ReadCodexFunding(context.Background(), testCodexID, path, c, func(e FundingEvidence) error { evs = append(evs, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return next, status, evs
}
func TestCodexSequenceTurnsRepeatedValuesAndPartialWrites(t *testing.T) {
	start := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn-a\"}}\n"
	q := quotaRecord(testCodexLimits)
	path := sequenceFixture(t, start+q+q+q[:len(q)/2])
	c, status, evs := readSequence(t, path, CodexCursor{})
	if status != "incomplete" || len(evs) != 2 {
		t.Fatalf("%s %+v", status, evs)
	}
	if evs[0].Attrs["turn_id"] != "turn-a" || evs[1].Attrs["turn_id"] != "turn-a" || evs[0].Attrs["observation_id"] == evs[1].Attrs["observation_id"] {
		t.Fatal(evs)
	}
	_, _, again := readSequence(t, path, c)
	if len(again) != 0 {
		t.Fatal("replayed acknowledged records")
	}
	appendRollout(t, path, q[len(q)/2:]+"{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":\"turn-a\"}}\n"+quotaRecord("null")+"{\"type\":\"turn_context\",\"payload\":{\"turn_id\":\"turn-b\",\"private\":\"never-export\"}}\n"+q)
	_, status, evs = readSequence(t, path, c)
	if status != "caught_up" || len(evs) != 3 || evs[0].Attrs["turn_id"] != "turn-a" || evs[1].Status != "unavailable" || evs[1].Attrs["turn_id"] != nil || evs[2].Attrs["turn_id"] != "turn-b" {
		t.Fatalf("%s %+v", status, evs)
	}
	raw, _ := json.Marshal(evs)
	if strings.Contains(string(raw), "never-export") || strings.Contains(string(raw), "total_tokens") {
		t.Fatal(string(raw))
	}
}
func TestCodexSequenceAppendFailureReplaysStableID(t *testing.T) {
	path := sequenceFixture(t, quotaRecord(testCodexLimits)+quotaRecord("null"))
	var ids []any
	c, _, err := ReadCodexFunding(context.Background(), testCodexID, path, CodexCursor{}, func(e FundingEvidence) error {
		ids = append(ids, e.Attrs["observation_id"])
		if len(ids) == 2 {
			return errors.New("disk full")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	_, _, evs := readSequence(t, path, c)
	if len(evs) != 1 || evs[0].Attrs["observation_id"] != ids[1] {
		t.Fatal(evs)
	}
	// Simulate append succeeding but the checkpoint being lost.
	_, _, all := readSequence(t, path, CodexCursor{})
	if all[0].Attrs["observation_id"] != ids[0] || all[1].Attrs["observation_id"] != ids[1] {
		t.Fatal("unstable replay IDs")
	}
}
func TestCodexSequenceBoundedBacklogAndOversizedRecord(t *testing.T) {
	path := sequenceFixture(t, strings.Repeat(quotaRecord(testCodexLimits), 300))
	c, status, evs := readSequence(t, path, CodexCursor{})
	if status != "backlog" || len(evs) != 256 {
		t.Fatalf("%s %d", status, len(evs))
	}
	c, status, evs = readSequence(t, path, c)
	if status != "caught_up" || len(evs) != 44 {
		t.Fatalf("%s %d", status, len(evs))
	}
	appendRollout(t, path, strings.Repeat("x", codexTailLimit+10)+"\n"+quotaRecord("null"))
	c, status, evs = readSequence(t, path, c)
	if status != "backlog" || len(evs) != 1 || evs[0].Attrs["gap_reason"] != "oversized_record" {
		t.Fatalf("%s %+v", status, evs)
	}
	_, status, evs = readSequence(t, path, c)
	if status != "caught_up" || len(evs) != 1 || evs[0].Status != "unavailable" {
		t.Fatalf("%s %+v", status, evs)
	}
}
func TestCodexSequenceReplacementAndArchive(t *testing.T) {
	path := sequenceFixture(t, quotaRecord(testCodexLimits))
	c, _, _ := readSequence(t, path, CodexCursor{})
	archived := filepath.Join(os.Getenv("CODEX_HOME"), "archived_sessions", filepath.Base(path))
	if err := os.MkdirAll(filepath.Dir(archived), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, archived); err != nil {
		t.Fatal(err)
	}
	next, _, evs := readSequence(t, archived, c)
	if next != c || len(evs) != 0 {
		t.Fatal("archive replayed records")
	}
	content, _ := os.ReadFile(archived)
	// Rewrite in place, with the same length and identity but changed checkpoint bytes.
	content = []byte(strings.Replace(string(content), "team", "plus", 1))
	if err := os.WriteFile(archived, content, 0o600); err != nil {
		t.Fatal(err)
	}
	// Change a byte near the checkpoint to establish a detectable rewrite.
	content = []byte(strings.Replace(string(content), "never-export", "other-export", 1))
	if err := os.WriteFile(archived, content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, evs = readSequence(t, archived, c)
	if len(evs) != 2 || evs[0].Status != "gap" || evs[1].Attrs["plan_type"] != "plus" {
		t.Fatal(evs)
	}
}

func TestCodexSequenceTruncationAndNoQuota(t *testing.T) {
	path := sequenceFixture(t, quotaRecord(testCodexLimits))
	c, _, _ := readSequence(t, path, CodexCursor{})
	b, _ := os.ReadFile(path)
	head, _, _ := strings.Cut(string(b), "\n")
	if err := os.WriteFile(path, []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, status, evs := readSequence(t, path, c)
	if status != "not_ready" || len(evs) != 1 || evs[0].Status != "gap" || next.TurnID != "" {
		t.Fatalf("%s %+v", status, evs)
	}
	// Appended explicit nulls are observations even without any quota values.
	appendRollout(t, path, quotaRecord("null"))
	_, status, evs = readSequence(t, path, next)
	if status != "caught_up" || len(evs) != 1 || evs[0].Status != "unavailable" {
		t.Fatalf("%s %+v", status, evs)
	}
}

func TestCodexSequenceSkippedPartialLineStaysIncomplete(t *testing.T) {
	path := sequenceFixture(t, quotaRecord(testCodexLimits))
	c, _, _ := readSequence(t, path, CodexCursor{})
	appendRollout(t, path, strings.Repeat("x", codexTailLimit))
	c, _, evs := readSequence(t, path, c)
	if len(evs) != 1 || evs[0].Status != "gap" {
		t.Fatal(evs)
	}
	_, status, evs := readSequence(t, path, c)
	if status != "incomplete" || len(evs) != 0 {
		t.Fatalf("%s %+v", status, evs)
	}
	appendRollout(t, path, "\n"+quotaRecord("null"))
	_, status, evs = readSequence(t, path, c)
	if status != "caught_up" || len(evs) != 1 || evs[0].Status != "unavailable" {
		t.Fatalf("%s %+v", status, evs)
	}
}
