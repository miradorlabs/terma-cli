//go:build unix

package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// Opening a FIFO or a symlink to a FIFO during readDir / below must never block, and below
// must still discover sibling checkouts beside the FIFO.
func TestReadDirAndBelowDoNotBlockOnFIFO(t *testing.T) {
	t.Parallel()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	link := filepath.Join(dir, "pipe-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	api := filepath.Join(dir, "acme-api")
	if err := os.MkdirAll(filepath.Join(api, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	errCh := make(chan string, 1)
	go func() {
		sc := newScan(dir, os.LookupEnv, work{deadline: start.Add(time.Second)})
		if entries := sc.readDir(fifo); len(entries) != 0 || sc.unknown {
			errCh <- "readDir(fifo) failed or marked unknown"
			return
		}
		if entries := sc.readDir(link); len(entries) != 0 || sc.unknown {
			errCh <- "readDir(fifo symlink) failed or marked unknown"
			return
		}
		sc.below(dir, maxBelowDepth+2)
		if !slices.Contains(sc.out, api) {
			errCh <- "below(dir) missed sibling acme-api beside FIFO"
			return
		}
		errCh <- ""
	}()
	select {
	case msg := <-errCh:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO scan blocked past timeout")
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("FIFO scan took %v, exceeded bound", took)
	}
}
