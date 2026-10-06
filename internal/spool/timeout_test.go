package spool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

func (s *Spool) size(t *testing.T) int64 {
	t.Helper()
	info, err := os.Stat(s.path())
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// Only the lock wait is charged the append budget, never the encode.
func TestAppendBudgetsOnlyTheLockWait(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	oversized := Event{Name: "large", Attrs: map[string]any{"data": strings.Repeat("x", 8<<20)}}
	if err := s.AppendContext(context.Background(), oversized); err != nil {
		t.Fatalf("a free lock must accept a slow encode: %v", err)
	}
	if n, _, err := s.Pending(); err != nil || n != 1 {
		t.Fatalf("the event must be queued: n=%d err=%v", n, err)
	}
	// A wait that really does outlast the budget is still refused.
	unlock, err := flock.Lock(context.Background(), filepath.Join(s.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := s.AppendContext(context.Background(), Event{Name: "blocked"}); err == nil {
		t.Fatal("a held lock must time the append out")
	}
	if n, _, err := s.Pending(); err != nil || n != 1 {
		t.Fatalf("a refused append must not queue anything: n=%d err=%v", n, err)
	}
}

func TestFlushDeadlineSpansBatches(t *testing.T) {
	// Serial: the first batch must be sent well inside the 50 ms deadline.
	s, _ := Open(t.TempDir())
	for _, name := range []string{"sent", "timed-out", "not-attempted"} {
		if err := s.Append(Event{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	var deadline time.Time
	res := s.Flush(context.Background(), SenderFunc(func(ctx context.Context, events []Event) ([]Event, error) {
		calls++
		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("send has no deadline")
		}
		if calls == 1 {
			deadline = got
			return nil, nil
		}
		if got != deadline {
			t.Fatal("deadline restarted for second batch")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}), FlushOptions{Batch: 1, Timeout: 50 * time.Millisecond})
	if !errors.Is(res.Err, context.DeadlineExceeded) || calls != 2 || res.Sent != 1 {
		t.Fatalf("result=%+v calls=%d", res, calls)
	}
	left, err := s.Peek(10)
	if err != nil || len(left) != 2 || left[0].Name != "timed-out" || left[1].Name != "not-attempted" {
		t.Fatalf("remaining events=%+v err=%v", left, err)
	}
}

func TestFlushCancellationBeforeAcknowledgementPreservesBatch(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "replay"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := s.Flush(ctx, SenderFunc(func(context.Context, []Event) ([]Event, error) {
		if err := s.Append(Event{Name: "late"}); err != nil {
			t.Fatal(err)
		}
		cancel() // accepted remotely, but no local acknowledgement yet
		return nil, nil
	}), FlushOptions{})
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("result=%+v", res)
	}
	left, err := s.Peek(10)
	if err != nil || len(left) != 2 || left[0].Name != "replay" || left[1].Name != "late" {
		t.Fatalf("events must survive for at-least-once retry: %+v err=%v", left, err)
	}
}

func TestFlushHeldRewriteFailurePreservesOriginalBatch(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "held"})
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		events[0].Attrs = map[string]any{"invalid": make(chan int)}
		return events, nil
	}), FlushOptions{})
	if res.Err == nil {
		t.Fatal("expected encoding failure")
	}
	left, err := s.Peek(10)
	if err != nil || len(left) != 1 || left[0].Name != "held" {
		t.Fatalf("held event lost before atomic rewrite: %+v err=%v", left, err)
	}
}

func TestPeekCannotPruneAnInflightBatch(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "in-flight"})
	// Big enough to exceed MaxBytes, small enough to encode quickly.
	oversized := Event{Name: "large", Attrs: map[string]any{"data": strings.Repeat("x", 2<<20)}}
	var encoded int64
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		line, err := json.Marshal(oversized)
		if err != nil {
			t.Fatal(err)
		}
		encoded = int64(len(line)) + 1
		if err := s.Append(oversized); err != nil {
			t.Fatal(err)
		}
		for i := int64(0); i <= MaxBytes/encoded+1; i++ {
			if err := s.Append(oversized); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Append(Event{Name: "newest"}); err != nil {
			t.Fatal(err)
		}
		if size := s.size(t); size <= MaxBytes {
			t.Fatalf("the queue must be over MaxBytes to prune: %d", size)
		}
		before, _ := os.ReadFile(s.path())
		if _, err := s.Peek(1); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(s.path())
		if string(before) != string(after) {
			t.Fatal("Peek mutated the prefix of an in-flight batch")
		}
		return nil, nil
	}), FlushOptions{})
	if res.Err != nil || res.Sent != 1 {
		t.Fatalf("result=%+v", res)
	}
	var names []string
	res = s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		for _, e := range events {
			names = append(names, e.Name)
		}
		return nil, nil
	}), FlushOptions{})
	// Pruning cuts to MaxBytes/2: it may remove some of what was appended, never the
	// newest event, and never re-reads the acknowledged prefix.
	if res.Err != nil || res.Pruned == 0 || res.Dropped != 0 {
		t.Fatalf("the oversized tail must prune, not fail: %+v", res)
	}
	if names[len(names)-1] != "newest" {
		t.Fatalf("prune must retain the newest event, sent=%v", names)
	}
	for _, name := range names {
		if name == "in-flight" {
			t.Fatal("the acknowledged prefix came back for a second delivery")
		}
	}
}

// A torn line and an expired event leave together but are counted apart.
func TestFlushSeparatesExpiryFromUnreadableLines(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "expired", Time: time.Now().Add(-2 * MaxAge)})
	f, err := os.OpenFile(s.path(), os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("malformed\n")
	_ = f.Close()
	res := s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) {
		t.Fatal("no valid events to send")
		return nil, nil
	}), FlushOptions{})
	if res.Err != nil || res.Expired != 1 || res.Dropped != 1 || res.Pruned != 0 {
		t.Fatalf("result=%+v", res)
	}
	if n, size, err := s.Pending(); err != nil || n != 0 || size != 0 {
		t.Fatalf("discarded lines left in queue: n=%d bytes=%d err=%v", n, size, err)
	}
}

func TestFlushDoesNotAcknowledgeAChangedPrefix(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	now := time.Now()
	_ = s.Append(Event{Name: "first", Time: now})
	res := s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) {
		// A flusher that takes only flush.lock acknowledges the event mid-send, then an append lands.
		if err := os.WriteFile(s.path(), nil, fileMode); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(Event{Name: "newer", Time: now}); err != nil {
			t.Fatal(err)
		}
		return nil, nil
	}), FlushOptions{})
	if res.Err == nil {
		t.Fatal("must reject an acknowledgement for a changed prefix")
	}
	left, err := s.Peek(10)
	if err != nil || len(left) != 1 || left[0].Name != "newer" {
		t.Fatalf("acknowledgement removed unrelated events: %+v err=%v", left, err)
	}
}
