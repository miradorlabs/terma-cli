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

// Swapping a traversed directory with a FIFO must never block walk, readSmallFile, or readDir.
func TestWalkAndOpenNeverBlockOnDirToFIFOSwap(t *testing.T) {
	t.Parallel()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	fifo := filepath.Join(dir, "fifo-src")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	sub := filepath.Join(dir, "real-sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "leaf"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(dir, "slot")
	link := filepath.Join(dir, "slot-link")
	tmpLink := filepath.Join(dir, "tmp-link")
	stop := make(chan struct{})
	done := make(chan struct{})
	swapErr := make(chan error, 1)
	go func() {
		defer close(done)
		toFIFO := false
		for {
			select {
			case <-stop:
				return
			default:
				toFIFO = !toFIFO
				_ = os.Remove(slot)
				if toFIFO {
					if err := syscall.Mkfifo(slot, 0o600); err != nil {
						swapErr <- err
						return
					}
				} else if err := os.Mkdir(slot, 0o700); err != nil {
					swapErr <- err
					return
				}
				_ = os.Remove(tmpLink)
				target := sub
				if toFIFO {
					target = fifo
				}
				if err := os.Symlink(target, tmpLink); err != nil {
					swapErr <- err
					return
				}
				if err := os.Rename(tmpLink, link); err != nil {
					swapErr <- err
					return
				}
			}
		}
	}()
	errCh := make(chan struct{}, 1)
	go func() {
		for range 40 {
			sc := newScan(dir, func(string) (string, bool) { return "", false }, work{deadline: time.Now().Add(2 * time.Second)})
			sc.candidate("slot/leaf", plain)
			sc.candidate("slot-link/leaf", plain)
			_ = sc.readDir(slot)
			_ = sc.readDir(link)
			_, _ = sc.readSmallFile(fifo)
			_, _ = sc.readSmallFile(link)
		}
		errCh <- struct{}{}
	}()
	select {
	case err := <-swapErr:
		close(stop)
		<-done
		t.Fatalf("swapper failed: %v", err)
	case <-errCh:
	case <-time.After(3 * time.Second):
		close(stop)
		<-done
		t.Fatal("dir-to-FIFO swap blocked walk/readDir/readSmallFile")
	}
	close(stop)
	<-done
}
