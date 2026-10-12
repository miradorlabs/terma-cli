package relay

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Stats counts what the relay did with every record (log record, span or data point), by reason.
// The counters' names leave the machine on the heartbeat, as terma.relay.heartbeat.counter.<name>:
// the platform reads them, so a name, once sent, is not renamed. Those named after an exporter's
// attribute (unclassified) leave only as their total.
type Stats struct {
	mu       sync.Mutex
	started  time.Time
	counters map[string]int
	// beat is the counters as of the last heartbeat that took them.
	beat map[string]int
}

func newStats() *Stats { return &Stats{started: time.Now(), counters: map[string]int{}} }

func (s *Stats) add(key string, n int) {
	s.mu.Lock()
	s.counters[key] += n
	s.mu.Unlock()
}

// maxUnclassified keeps a hostile or broken exporter from growing the stats without limit.
const maxUnclassified = 256

// takeSinceBeat is what was counted since the last beat that took the counters, which
// now covers it too.
func (s *Stats) takeSinceBeat() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	delta := map[string]int{}
	for k, v := range s.counters {
		if n := v - s.beat[k]; n != 0 {
			delta[k] = n
		}
	}
	s.beat = maps.Clone(s.counters)
	return delta
}

// untake hands back what a beat that failed took, for the next beat to report.
func (s *Stats) untake(delta map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, n := range delta {
		s.beat[k] -= n
	}
}

func (s *Stats) unclassified(key string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := "unclassified." + key
	if _, ok := s.counters[name]; !ok {
		distinct := 0
		for k := range s.counters {
			if strings.HasPrefix(k, "unclassified.") {
				distinct++
			}
		}
		if distinct >= maxUnclassified {
			name = "unclassified_overflow"
		}
	}
	s.counters[name] += n
}

func (s *Stats) received(sig Signal, n int)  { s.add("received."+string(sig), n) }
func (s *Stats) forwarded(sig Signal, n int) { s.add("forwarded."+string(sig), n) }
func (s *Stats) dropped(sig Signal, reason string, n int) {
	s.add("dropped."+reason+"."+string(sig), n)
}

// Snapshot is the stats as they stand.
type Snapshot struct {
	Since    time.Time      `json:"since"`
	Counters map[string]int `json:"counters"`
}

// Snapshot copies the counters.
func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := make(map[string]int, len(s.counters))
	maps.Copy(c, s.counters)
	return Snapshot{Since: s.started, Counters: c}
}

// Keys lists the counters in order, for printing.
func (s Snapshot) Keys() []string {
	keys := make([]string, 0, len(s.Counters))
	for k := range s.Counters {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
