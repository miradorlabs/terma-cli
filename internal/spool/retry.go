package spool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// NextAttempt is when the spool-wide backoff ends; zero when none is in force.
func (s *Spool) NextAttempt() time.Time {
	next, _ := s.backoff()
	return next
}

// recordFailure doubles the previous wait (30s..1h); the file holds
// "<next-unix> <wait-seconds>" so the window survives between processes.
func (s *Spool) recordFailure(now time.Time) {
	wait := minBackoff
	if _, prev := s.backoff(); prev > 0 {
		wait = prev * 2
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	_ = os.WriteFile(filepath.Join(s.dir, backoffFile),
		[]byte(strconv.FormatInt(now.Add(wait).Unix(), 10)+" "+strconv.FormatInt(int64(wait/time.Second), 10)),
		fileMode)
}

func (s *Spool) backoff() (next time.Time, wait time.Duration) {
	data, err := os.ReadFile(filepath.Join(s.dir, backoffFile))
	if err != nil {
		return time.Time{}, 0
	}
	fields := bytes.Fields(data)
	if len(fields) == 0 {
		return time.Time{}, 0
	}
	unix, err := strconv.ParseInt(string(fields[0]), 10, 64)
	if err != nil {
		return time.Time{}, 0
	}
	next = time.Unix(unix, 0)
	if len(fields) > 1 {
		if secs, err := strconv.ParseInt(string(fields[1]), 10, 64); err == nil {
			wait = time.Duration(secs) * time.Second
		}
	}
	return next, wait
}

func (s *Spool) clearBackoff() {
	_ = os.Remove(filepath.Join(s.dir, backoffFile))
}

// LastFlush is when a flush last ran to completion, or zero.
func (s *Spool) LastFlush() time.Time {
	data, err := os.ReadFile(filepath.Join(s.dir, lastFlushFile))
	if err != nil {
		return time.Time{}
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

func (s *Spool) recordFlush(now time.Time) {
	_ = os.WriteFile(filepath.Join(s.dir, lastFlushFile), []byte(strconv.FormatInt(now.Unix(), 10)), fileMode)
}
