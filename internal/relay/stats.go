package relay

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// stats are the relay's counters since it started, reported by /healthz.
type stats struct {
	mu        sync.Mutex
	received  int64
	routed    int64
	delivered int64
	dead      int64
	dropped   int64
	rejected  int64
	held      map[string]bool
	errors    map[string]string // project → last delivery failure
	lastError string
	lastAt    time.Time
}

func newStats() *stats {
	return &stats{held: map[string]bool{}, errors: map[string]string{}}
}

func (s *stats) addReceived(n int)  { s.add(&s.received, n) }
func (s *stats) addRouted(n int)    { s.add(&s.routed, n) }
func (s *stats) addDelivered(n int) { s.add(&s.delivered, n) }
func (s *stats) addDead(n int)      { s.add(&s.dead, n) }
func (s *stats) addDropped(n int)   { s.add(&s.dropped, n) }

func (s *stats) addRejected(n int64) {
	s.mu.Lock()
	s.rejected += n
	s.mu.Unlock()
}

func (s *stats) add(field *int64, n int) {
	s.mu.Lock()
	*field += int64(n)
	s.mu.Unlock()
}

func (s *stats) setHeld(route string, held bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held {
		s.held[route] = true
	} else {
		delete(s.held, route)
	}
}

func (s *stats) setError(project, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors[project] = detail
	s.lastError = project + ": " + detail
	s.lastAt = time.Now()
}

func (s *stats) clearError(project string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.errors, project)
	if len(s.errors) == 0 {
		s.lastError = ""
	}
}

// Counters are the relay's running totals, in items (log records, spans, data points)
// except Dead and Dropped, which count request bodies.
type Counters struct {
	Received  int64 `json:"received"`
	Routed    int64 `json:"routed"`
	Delivered int64 `json:"delivered"`
	Dead      int64 `json:"dead"`
	Dropped   int64 `json:"dropped"`
	// Rejected counts records the gateway dropped from requests it otherwise accepted
	// (OTLP partial success).
	Rejected int64 `json:"rejected"`
}

func (s *stats) snapshot() (Counters, []string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := slices.Sorted(maps.Keys(s.held))
	return Counters{Received: s.received, Routed: s.routed, Delivered: s.delivered, Dead: s.dead, Dropped: s.dropped, Rejected: s.rejected},
		held, s.lastError
}
