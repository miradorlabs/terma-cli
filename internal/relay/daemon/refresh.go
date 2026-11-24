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
	// Every, when set, overrides Interval for team (e.g. RefusedRecheck for Refused teams).
	Every func(team string) time.Duration
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
	every    map[string]time.Duration
	inFlight map[string]bool
	pending  map[string]bool
	wake     chan struct{}
}

func (p *PolicyRefresher) wakeChan() chan struct{} {
	if p.wake == nil {
		p.wake = make(chan struct{}, 1)
	}
	return p.wake
}

func (p *PolicyRefresher) intervalFor(team string) time.Duration {
	if p.Every != nil {
		if d := p.Every(team); d > 0 {
			return d
		}
	}
	return p.Interval
}

// Nudge requests an immediate coalesced refresh for team. If a refresh for team is already
// in flight, one follow-up refresh runs as soon as it finishes; multiple nudges coalesce.
func (p *PolicyRefresher) Nudge(team string) {
	if team == "" {
		return
	}
	p.mu.Lock()
	if p.next == nil {
		p.next = map[string]time.Time{}
	}
	if p.every == nil {
		p.every = map[string]time.Duration{}
	}
	if p.pending == nil {
		p.pending = map[string]bool{}
	}
	if p.inFlight[team] {
		p.pending[team] = true
	} else {
		p.next[team] = time.Time{}
		delete(p.every, team)
	}
	ch := p.wakeChan()
	p.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run refreshes policies until ctx ends, and waits for the fetches it started.
func (p *PolicyRefresher) Run(ctx context.Context) {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.After == nil {
		p.After = time.After
	}
	p.mu.Lock()
	if p.next == nil {
		p.next = map[string]time.Time{}
	}
	if p.every == nil {
		p.every = map[string]time.Duration{}
	}
	if p.inFlight == nil {
		p.inFlight = map[string]bool{}
	}
	if p.pending == nil {
		p.pending = map[string]bool{}
	}
	wake := p.wakeChan()
	p.mu.Unlock()
	var fetches sync.WaitGroup
	defer fetches.Wait()
	for {
		for _, team := range p.due() {
			fetches.Go(func() {
				_ = p.Refresh(ctx, team)
				every := p.intervalFor(team)
				p.mu.Lock()
				p.inFlight[team] = false
				if p.pending[team] {
					delete(p.pending, team)
					p.next[team] = time.Time{}
					delete(p.every, team)
					p.mu.Unlock()
					select {
					case wake <- struct{}{}:
					default:
					}
					return
				}
				p.every[team] = every
				p.next[team] = p.Now().Add(every)
				p.mu.Unlock()
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-p.After(p.Discover):
		}
	}
}

func (p *PolicyRefresher) due() []string {
	teams := p.Teams()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.Now()
	var out []string
	for _, team := range teams {
		if p.inFlight[team] {
			continue
		}
		curEvery := p.intervalFor(team)
		next, known := p.next[team]
		if !known || (!next.IsZero() && p.every[team] != curEvery) {
			next = now
			if at := p.Fetched(team); !at.IsZero() {
				next = at.Add(curEvery)
			}
			p.every[team] = curEvery
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
