//go:build unix

package harness

import (
	"fmt"
	"sync"
	"testing"
)

// One record file serves every Claude config on the machine, and an install read it,
// set its own entry and renamed its copy back: two at once and the later rename forgot
// the status line the other had displaced.
func TestConcurrentStatusLineRecordsKeepEveryConfig(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const configs = 16

	var wg sync.WaitGroup
	for i := range configs {
		wg.Go(func() {
			path := fmt.Sprintf("/home/dev/claude-%02d/settings.json", i)
			if err := saveStatusLineRecord(path, &statusLineRecord{}); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	recs, err := loadStatusLineRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != configs {
		t.Fatalf("the record kept %d of %d configs", len(recs), configs)
	}
}
