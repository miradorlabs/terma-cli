package relay

import (
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

// Stats counts what the relay did with every record, by reason, so a spike run (and
// the live canary) can say exactly what was forwarded and why the rest was not. A
// record is a log record, a span or a metric data point.
type Stats struct {
	mu       sync.Mutex
	started  time.Time
	counters map[string]int
}

func newStats() *Stats { return &Stats{started: time.Now(), counters: map[string]int{}} }

func (s *Stats) add(key string, n int) {
	s.mu.Lock()
	s.counters[key] += n
	s.mu.Unlock()
}

// maxUnclassified bounds how many distinct unclassified keys are counted by name: a
// hostile or broken exporter must not grow the stats without limit.
const maxUnclassified = 256

// unclassified counts a key the content gate dropped for not being classified, by name
// while fewer than maxUnclassified are, then as unclassified_overflow.
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

// Snapshot is the stats as they stand, for GET /stats and stats.json.
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
	sort.Strings(keys)
	return keys
}
