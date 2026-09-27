package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Per-destination retry windows.
//
// One spool-wide backoff made every destination wait out the worst one's. A project
// whose key its ingest host refused failed every pass, the window doubled to an hour,
// and hook-started flushes, which honour it, delivered every other project's events
// once an hour for the two weeks the refused ones took to expire. A Sender that
// routes a batch to several destinations keeps a window for each instead and returns
// a PartialDelivery, which opens no spool-wide window.
//
// Only a Sender calls the writers, from inside Flush: the delivery lock Flush holds is
// what makes their read, edit and rename safe. Readers take nothing, since every write
// is an atomic rename.

const retryFile = "retry.json"

// retryWindow is one destination's backoff: when it may next be tried, and the wait
// that produced it, so the next failure can double it.
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
	// A file that does not decode costs a retry sooner than planned, nothing more.
	_ = json.Unmarshal(data, &windows)
	return windows
}

// saveRetryWindows forgets a window closed for longer than MaxAge: every event that
// could have been waiting on it has expired.
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

// RetryAt is when dest may next be sent to; zero when its last send, if any, went
// through.
func (s *Spool) RetryAt(dest string) time.Time {
	w, ok := s.loadRetryWindows()[dest]
	if !ok {
		return time.Time{}
	}
	return time.Unix(w.Next, 0)
}

// DestinationFailed records a failed send to dest and returns when it may be tried
// again: the spool-wide policy, per destination — 30 seconds after the first failure,
// doubling to an hour.
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

// DestinationDelivered closes dest's window: its next failure starts again at 30
// seconds.
func (s *Spool) DestinationDelivered(dest string, now time.Time) {
	windows := s.loadRetryWindows()
	if _, ok := windows[dest]; !ok {
		return
	}
	delete(windows, dest)
	s.saveRetryWindows(windows, now)
}

// RetryWindows lists the destinations whose window is open at now, with when each
// may next be tried.
func (s *Spool) RetryWindows(now time.Time) map[string]time.Time {
	open := map[string]time.Time{}
	for dest, w := range s.loadRetryWindows() {
		if next := time.Unix(w.Next, 0); now.Before(next) {
			open[dest] = next
		}
	}
	return open
}
