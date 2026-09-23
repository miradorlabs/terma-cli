//go:build unix

package keystore

import (
	"fmt"
	"sync"
	"testing"
)

// Two installs in two repositories each add their own project's key. Unlocked, each
// read the file, added one key and renamed its copy back, and the later rename forgot
// the earlier key.
func TestConcurrentSetsKeepEveryKey(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const key = "ter_srv_0123456789abcdef"
	const writers = 16

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			if err := Set(fmt.Sprintf("proj-%02d", i), key); err != nil {
				t.Errorf("set %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	if got := len(Projects()); got != writers {
		t.Fatalf("the keystore kept %d of %d keys", got, writers)
	}
}
