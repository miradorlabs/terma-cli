package relayexport

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithTemplateLockCanonicalizesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink test")
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	realTarget := filepath.Join(realDir, "plugin.js")
	linkTarget := filepath.Join(linkDir, "plugin.js")

	var active, overlap atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 4 {
		wg.Add(1)
		target := realTarget
		if i%2 == 1 {
			target = linkTarget
		}
		go func(p string) {
			defer wg.Done()
			for range 5 {
				err := WithTemplateLock(p, func() error {
					if active.Add(1) > 1 {
						overlap.Add(1)
					}
					time.Sleep(2 * time.Millisecond)
					active.Add(-1)
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(target)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("WithTemplateLock error: %v", err)
	}
	if overlap.Load() != 0 {
		t.Fatalf("symlinked paths did not share the same template lock (overlaps=%d)", overlap.Load())
	}
}
