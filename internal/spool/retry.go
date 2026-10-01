package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Per-destination retry windows, so one refused project does not hold back the others.
// Only a Sender inside Flush writes them: the delivery lock makes the read-edit-rename safe.

const retryFile = "retry.json"

// retryWindow keeps the wait that produced Next, so the next failure can double it.
type retryWindow struct {
	Next int64 `json:"next"` // unix seconds
	Wait int64 `json:"wait"` // seconds
}

func (s *Spool) loadRetryWindows() map[string]retryWindow {
	windows := map[string]retryWindow{}
	data, err := os.ReadFile(filepath.Join(s.dir, retryFile))
	if err != nil {
		return windows
	}
	_ = json.Unmarshal(data, &windows)
	return windows
}

// saveRetryWindows forgets windows closed longer than MaxAge: their events have expired.
func (s *Spool) saveRetryWindows(windows map[string]retryWindow, now time.Time) {
	for dest, w := range windows {
		if now.Sub(time.Unix(w.Next, 0)) > MaxAge {
			delete(windows, dest)
		}
	}
	path := filepath.Join(s.dir, retryFile)
	if len(windows) == 0 {
		_ = os.Remove(path)
		return
	}
	data, err := json.Marshal(windows)
	if err != nil {
		return
	}
	_ = config.WriteFileAtomicNoSync(path, data, fileMode)
}

// RetryAt is when dest may next be sent to; zero when no window is open.
func (s *Spool) RetryAt(dest string) time.Time {
	w, ok := s.loadRetryWindows()[dest]
	if !ok {
		return time.Time{}
	}
	return time.Unix(w.Next, 0)
}

// DestinationFailed records a failed send to dest and returns when it may be retried (30s doubling to 1h).
func (s *Spool) DestinationFailed(dest string, now time.Time) time.Time {
	windows := s.loadRetryWindows()
	wait := minBackoff
	if prev := windows[dest].Wait; prev > 0 {
		wait = time.Duration(prev) * time.Second * 2
	}
	wait = min(wait, maxBackoff)
	next := now.Add(wait)
	windows[dest] = retryWindow{Next: next.Unix(), Wait: int64(wait / time.Second)}
	s.saveRetryWindows(windows, now)
	return time.Unix(next.Unix(), 0)
}

// DestinationDelivered closes dest's window.
func (s *Spool) DestinationDelivered(dest string, now time.Time) {
	windows := s.loadRetryWindows()
	if _, ok := windows[dest]; !ok {
		return
	}
	delete(windows, dest)
	s.saveRetryWindows(windows, now)
}

// RetryWindows lists the destinations whose window is open at now, with each one's end.
func (s *Spool) RetryWindows(now time.Time) map[string]time.Time {
	open := map[string]time.Time{}
	for dest, w := range s.loadRetryWindows() {
		if next := time.Unix(w.Next, 0); now.Before(next) {
			open[dest] = next
		}
	}
	return open
}
