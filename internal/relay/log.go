package relay

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxLogBytes is where relay.log is rotated to relay.log.1; two files at most.
const maxLogBytes = 2 << 20

// Log is the relay's diagnostic log, relay.log in the state directory. A service manager
// discards the relay's output, so this file is where `terma relay status` and a person
// debugging look.
type Log struct {
	mu   sync.Mutex
	path string
}

// NewLog returns the log in dir.
func NewLog(dir string) *Log {
	return &Log{path: filepath.Join(dir, "relay.log")}
}

// Path is the log file.
func (l *Log) Path() string { return l.path }

// Printf appends one timestamped line. Failures are ignored: a relay that cannot write
// its log still relays.
func (l *Log) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.path); err == nil && st.Size() > maxLogBytes {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
