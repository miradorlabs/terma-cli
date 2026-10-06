package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunningCodexDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if _, ok := runningDaemon(); ok {
		t.Fatal("no record, yet a daemon")
	}
	dir := filepath.Join(home, "app-server-daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(pid int, start int64) {
		rec := fmt.Sprintf(`{"pid":%d,"processStartTime":"x","processIdentity":{"bootId":"b","uniqueId":1,"startSeconds":%d,"startMicroseconds":5}}`, pid, start)
		if err := os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(os.Getpid(), 1790672124)
	d, ok := runningDaemon()
	if !ok || d.PID != os.Getpid() || !d.Started.Equal(time.Unix(1790672124, 0)) {
		t.Fatalf("daemon = %+v, %v", d, ok)
	}
	// A record left behind by a daemon that exited names no live process.
	write(1<<22-3, 1790672124)
	if _, ok := runningDaemon(); ok {
		t.Fatal("a dead daemon's record counted as running")
	}
}
