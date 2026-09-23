package spool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// size reports the queue's on-disk size, which is what the disk bound measures.
func (s *Spool) size(t *testing.T) int64 {
	t.Helper()
	info, err := os.Stat(s.path())
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// Nothing but the lock wait is charged the append budget. Encoding is not, because
// the budget has to survive an event that is large to write and a machine that is
// busy doing it — an event refused there is lost while the lock it wanted was free.
func TestAppendBudgetsOnlyTheLockWait(t *testing.T) {
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

func TestFlushStalledHTTPAllowsAppendAndRetainsQueue(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := s.Append(Event{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	result := make(chan Result, 1)
	go func() {
		result <- s.Flush(context.Background(), &OTLPSender{Endpoint: srv.URL, APIKey: "test-key"}, FlushOptions{Timeout: 500 * time.Millisecond})
	}()
	select {
	case <-entered:
	case res := <-result:
		t.Fatalf("flush returned before reaching endpoint: %+v", res)
	case <-time.After(3 * time.Second):
		t.Fatal("sender never reached endpoint")
	}
	// This uses the real append lock while the network request is stalled.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.AppendContext(ctx, Event{Name: "during-send"}); err != nil {
		t.Fatalf("network delivery blocked append: %v", err)
	}
	select {
	case res := <-result:
		if !errors.Is(res.Err, context.DeadlineExceeded) || res.Sent != 0 {
			t.Fatalf("flush result: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("flush ignored its deadline")
	}
	if s.NextAttempt().IsZero() {
		t.Fatal("failed delivery must back off")
	}
	var names []string
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		for _, e := range events {
			names = append(names, e.Name)
		}
		return nil, nil
	}), FlushOptions{Force: true})
	if res.Err != nil || strings.Join(names, ",") != "first,during-send" {
		t.Fatalf("retry lost/reordered events: %+v names=%v", res, names)
	}
	if n, _, err := s.Pending(); err != nil || n != 0 {
		t.Fatalf("queue after retry: n=%d err=%v", n, err)
	}
}

func TestFlushDeadlineSpansBatches(t *testing.T) {
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
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "in-flight"})
	// An event big enough to exceed MaxBytes but small enough to encode inside the
	// append lock budget — the real bound, which AppendContext now charges only the
	// lock wait against. A larger one would be refused here for encoder time, which
	// is a different test (TestAppendBudgetsOnlyTheLockWait).
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
	// Pruning cuts to MaxBytes/2, so it removes a prefix of what the in-flight send
	// appended rather than all of it. What it must never remove is the newest event,
	// and it must never re-read the prefix the first flush acknowledged.
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

// A line that will not decode is a torn write; an event past MaxAge is time. The
// two leave the queue together but must not be reported as the same thing: only
// one of them means the file was unreadable.
func TestFlushSeparatesExpiryFromUnreadableLines(t *testing.T) {
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
	s, _ := Open(t.TempDir())
	now := time.Now()
	_ = s.Append(Event{Name: "first", Time: now})
	res := s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) {
		// An older flusher uses only flush.lock. Model it acknowledging the
		// original event while our network request is in flight, then an append.
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
