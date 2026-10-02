package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogFile is the relay's own log of what an operator should see: starts and exits with
// their counters, refused deliveries, retries, failed heartbeats. It is written by the
// relay itself, so a hook-started relay, whose output goes nowhere, keeps one too.
const LogFile = "relay.log"

// DaemonLogFile takes a relay's stdout and stderr: the service manager points the
// service's there, and Spawn a hook-started relay's, so a crash leaves its trace.
const DaemonLogFile = "daemon.log"

// maxLog bounds each log; past it, the log is moved to <name>.1, replacing the older one.
const maxLog = 1 << 20

// Log appends timestamped lines to dir's relay log, opening it on the first line.
type Log struct {
	path string
	mu   sync.Mutex
	f    *os.File
	size int64
}

// NewLog is the relay log in dir; nothing is opened until a line is written.
func NewLog(dir string) *Log { return &Log{path: filepath.Join(dir, LogFile)} }

// Printf writes one line; a log that cannot be written is skipped, never fatal.
func (l *Log) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	line := time.Now().UTC().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil && !l.open() {
		return
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
	if l.size >= maxLog {
		_ = l.f.Close()
		l.f = nil
		_ = os.Rename(l.path, l.path+".1")
	}
}

// Close closes the log file, if one is open.
func (l *Log) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

func (l *Log) open() bool {
	if info, err := os.Stat(l.path); err == nil && info.Size() >= maxLog {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return false
	}
	l.f, l.size = f, info.Size()
	return true
}

// openDaemonLog opens dir's daemon log for a relay's output, emptying one past its bound
// first: every writer appends, so the next write lands at the start.
func openDaemonLog(dir string) *os.File {
	path := filepath.Join(dir, DaemonLogFile)
	if info, err := os.Stat(path); err == nil && info.Size() >= maxLog {
		_ = os.Truncate(path, 0)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
