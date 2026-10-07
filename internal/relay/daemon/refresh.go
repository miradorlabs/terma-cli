package daemon

import (
	"context"
	"sync"
	"time"
)

// PolicyRefresher keeps each team's collection policy fresh while the relay runs, one fetch
// in flight per team; a failed fetch leaves the last validated policy in force.
type PolicyRefresher struct {
	Interval time.Duration
	// Discover is how often the teams are listed, so a newly connected one is found.
	Discover time.Duration
	Teams    func() []string
	// Fetched is when team's validated policy was fetched, zero when there is none.
	Fetched func(team string) time.Time
	Refresh func(ctx context.Context, team string) error
	// Now and After are the clock; time.Now and time.After when nil.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	mu       sync.Mutex
	next     map[string]time.Time
	inFlight map[string]bool
}

// Run refreshes policies until ctx ends, and waits for the fetches it started.
func (p *PolicyRefresher) Run(ctx context.Context) {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.After == nil {
		p.After = time.After
	}
	p.next, p.inFlight = map[string]time.Time{}, map[string]bool{}
	var fetches sync.WaitGroup
	defer fetches.Wait()
	for {
		started := p.Now()
		for _, team := range p.due() {
			fetches.Go(func() {
				_ = p.Refresh(ctx, team)
				p.mu.Lock()
				p.inFlight[team] = false
				// From the start, so a slow fetch does not stretch the cadence.
				p.next[team] = started.Add(p.Interval)
				p.mu.Unlock()
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-p.After(p.Discover):
		}
	}
}

func (p *PolicyRefresher) due() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.Now()
	var out []string
	for _, team := range p.Teams() {
		if p.inFlight[team] {
			continue
		}
		next, known := p.next[team]
		if !known {
			next = now
			if at := p.Fetched(team); !at.IsZero() {
				next = at.Add(p.Interval)
			}
		}
		if now.Before(next) {
			p.next[team] = next
			continue
		}
		p.inFlight[team] = true
		out = append(out, team)
	}
	return out
}
