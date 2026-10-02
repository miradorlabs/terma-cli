package claim

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Diagnostic only: measures lost writers over many rounds, and whether a replace fails
// while another handle reads the target.
func TestDiagLostWriters(t *testing.T) {
	enable(t)
	saved := lockWait
	lockWait = time.Minute
	t.Cleanup(func() { lockWait = saved })
	var lostRounds, falseWrites atomic.Int32
	for round := range 50 {
		sid := "s" + string(rune('a'+round%26)) + string(rune('a'+round/26))
		now := time.Now()
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if !Write(sid, Claim{ProjectID: "p", PIDs: []int{1000 + i}}, now) {
					falseWrites.Add(1)
				}
			}()
		}
		wg.Wait()
		c, _ := Read(sid, now)
		for i := range 16 {
			if !c.Covers(1000 + i) {
				lostRounds.Add(1)
				break
			}
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	_ = os.WriteFile(p, []byte("a"), 0o600)
	f, _ := os.Open(p)
	err := config.WriteFileAtomicNoSync(p, []byte("b"), 0o600)
	f.Close()
	t.Errorf("DIAG rounds-with-loss=%d/50 write-false=%d/800 replace-while-open err=%v", lostRounds.Load(), falseWrites.Load(), err)
}
