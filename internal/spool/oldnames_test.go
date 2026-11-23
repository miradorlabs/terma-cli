package spool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// olderEventsFile is where a terma before user tokens (v0.3.x) queues its events.
const olderEventsFile = "events.jsonl"

// An older terma's queue is fenced off both ways: a flush never reads or sends what it
// holds, even a line that names an account, and an append never writes there.
func TestAnOlderTermasQueueIsNeitherReadNorWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	older := []byte(`{"name":"older","time":"2026-11-20T00:00:00Z","account":"aaaaaaaaaaaaaaaa"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, olderEventsFile), older, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Bind("aaaaaaaaaaaaaaaa")
	if err := s.Append(Event{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		for _, e := range events {
			sent = append(sent, e.Name)
		}
		return nil, nil
	}), FlushOptions{Force: true})
	if res.Err != nil || len(sent) != 1 || sent[0] != "new" {
		t.Fatalf("sent %v (%+v); want only this version's event", sent, res)
	}
	if data, err := os.ReadFile(filepath.Join(dir, olderEventsFile)); err != nil || !bytes.Equal(data, older) {
		t.Fatalf("the older queue = %q, %v; want it untouched", data, err)
	}
}
