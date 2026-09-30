package daemon

import (
	"context"
	"sync"
	"time"
)

// PolicyRefresher keeps each team's collection policy fresh while the relay runs: every
// Discover it asks which teams there are, and refreshes each whose last attempt is
// Interval old, at most one fetch in flight per team. A refused fetch waits as long as
// a successful one before the next attempt, and leaves the last validated policy in
// force (Refresh never replaces it with anything unvalidated). A team found while
// another's fetch hangs is refreshed without waiting for it.
type PolicyRefresher struct {
	// Interval is how often a team's policy is fetched.
	Interval time.Duration
	// Discover is how often the teams are listed, so a newly connected one is found.
	Discover time.Duration
	// Teams lists the teams whose policy to keep: the scopes the machine exports for.
	Teams func() []string
	// Fetched is when team's validated policy was fetched, zero when there is none.
	Fetched func(team string) time.Time
	// Refresh fetches, validates and saves team's policy.
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
		for _, team := range p.due() {
			fetches.Go(func() {
				_ = p.Refresh(ctx, team)
				p.mu.Lock()
				p.inFlight[team] = false
				p.next[team] = p.Now().Add(p.Interval)
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

// due marks and returns the teams to fetch now.
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
