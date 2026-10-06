//go:build unix

package claude

import (
	"fmt"
	"sync"
	"testing"
)

// Concurrent installs under different config dirs each keep the status line they displaced.
func TestConcurrentStatusLineRecordsKeepEveryConfig(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	const configs = 16

	var wg sync.WaitGroup
	for i := range configs {
		wg.Go(func() {
			path := fmt.Sprintf("/home/dev/claude-%02d/settings.json", i)
			if err := saveStatusLineRecord(configDir, path, &statusLineRecord{}); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	recs, err := loadStatusLineRecords(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != configs {
		t.Fatalf("the record kept %d of %d configs", len(recs), configs)
	}
}
