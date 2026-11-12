package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Holds are apart from retry windows: a destination held waits for something it needs (a
// key, a policy) that a later look may find, so a hold only says when looking again is
// worth starting a flush for. A flusher looks whenever it runs, and a hold never delays it.

const holdFile = "held.json"

// Hold marks dests held until until, in one write. Only a flusher, under the delivery lock,
// writes holds.
func (s *Spool) Hold(until time.Time, dests ...string) {
	if len(dests) == 0 {
		return
	}
	held := s.loadHolds()
	for _, d := range dests {
		held[d] = until.Unix()
	}
	s.saveHolds(held, time.Now())
}

// Unhold takes dest's hold away: what it waited for is there.
func (s *Spool) Unhold(dest string) {
	held := s.loadHolds()
	if _, ok := held[dest]; ok {
		delete(held, dest)
		s.saveHolds(held, time.Now())
	}
}

// Holds lists the destinations held at now, with when each hold ends.
func (s *Spool) Holds(now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for d, until := range s.loadHolds() {
		if t := time.Unix(until, 0); now.Before(t) {
			out[d] = t
		}
	}
	return out
}

func (s *Spool) loadHolds() map[string]int64 {
	held := map[string]int64{}
	if data, err := os.ReadFile(filepath.Join(s.dir, holdFile)); err == nil {
		_ = json.Unmarshal(data, &held)
	}
	return held
}

// saveHolds forgets holds that ended.
func (s *Spool) saveHolds(held map[string]int64, now time.Time) {
	for d, until := range held {
		if !now.Before(time.Unix(until, 0)) {
			delete(held, d)
		}
	}
	path := filepath.Join(s.dir, holdFile)
	if len(held) == 0 {
		_ = os.Remove(path)
		return
	}
	if data, err := json.Marshal(held); err == nil {
		_ = config.WriteFileAtomicNoSync(path, data, fileMode)
	}
}
