//go:build unix

package keystore

import (
	"fmt"
	"sync"
	"testing"
)

// Concurrent installs in two repositories each keep their own project's key.
func TestConcurrentSetsKeepEveryKey(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const key = "ter_srv_0123456789abcdef"
	const writers = 16

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			if err := Set(fmt.Sprintf("proj-%02d", i), key, Hosts{}); err != nil {
				t.Errorf("set %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	for i := range writers {
		if Get(fmt.Sprintf("proj-%02d", i)) != key {
			t.Errorf("the keystore lost proj-%02d's key", i)
		}
	}
}
