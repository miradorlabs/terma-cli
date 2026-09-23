//go:build unix

package spool

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

func TestSpoolLockWaitsRespectDeadlines(t *testing.T) {
	for _, filename := range []string{lockFile, deliveryLockFile} {
		t.Run(filename, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			_ = s.Append(Event{Name: "pending"})
			unlock, err := flock.Lock(context.Background(), filepath.Join(s.dir, filename))
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			res := s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) {
				t.Fatal("send must not start without its locks")
				return nil, nil
			}), FlushOptions{Timeout: 30 * time.Millisecond})
			if !errors.Is(res.Err, context.DeadlineExceeded) {
				t.Fatalf("flush result=%+v", res)
			}
			if filename == lockFile {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				defer cancel()
				if err := s.AppendContext(ctx, Event{Name: "blocked"}); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("append error=%v", err)
				}
				if err := s.Append(Event{Name: "also-blocked"}); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("append without caller deadline must be bounded: %v", err)
				}
			}
			if n, _, _ := s.Pending(); n != 1 {
				t.Fatalf("lock timeout changed queue: %d events", n)
			}
		})
	}
}

func TestConcurrentFlushWaitsWithoutResending(t *testing.T) {
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "once"})
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	first := make(chan Result, 1)
	go func() {
		first <- s.Flush(context.Background(), SenderFunc(func(ctx context.Context, events []Event) ([]Event, error) {
			close(entered)
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}), FlushOptions{Timeout: 3 * time.Second})
	}()
	<-entered
	res := s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) {
		t.Fatal("concurrent flusher sent the same batch")
		return nil, nil
	}), FlushOptions{Timeout: 30 * time.Millisecond})
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("second flush=%+v", res)
	}
	// A waiting flusher must not hold the append lock either.
	if err := s.Append(Event{Name: "late"}); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	if res := <-first; res.Err != nil || res.Sent != 1 {
		t.Fatalf("first flush=%+v", res)
	}
	left, err := s.Peek(10)
	if err != nil || len(left) != 1 || left[0].Name != "late" {
		t.Fatalf("late append lost: %+v err=%v", left, err)
	}
}
