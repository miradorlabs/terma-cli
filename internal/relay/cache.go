package relay

import (
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// lookupCache makes a busy session cost one file read a second rather than one per part,
// with deliverMu held.
type lookupCache struct {
	mu       sync.Mutex
	claims   map[string]cachedClaim
	policies map[string]cachedPolicy
}

type cachedClaim struct {
	c  claim.Claim
	ok bool
	at time.Time
}

type cachedPolicy struct {
	pol Policy
	ok  bool
	at  time.Time
}

func (r *Relay) lookup(session string) (claim.Claim, bool) {
	now := r.opts.Now()
	if ttl := r.opts.ClaimCacheTTL; ttl > 0 {
		r.cache.mu.Lock()
		e, hit := r.cache.claims[session]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl {
			return e.c, e.ok
		}
	}
	c, ok := r.opts.Lookup(session, now)
	if r.opts.ClaimCacheTTL > 0 {
		r.cache.mu.Lock()
		r.cache.claims[session] = cachedClaim{c, ok, now}
		r.cache.mu.Unlock()
	}
	return c, ok
}

func (r *Relay) resolve(c claim.Claim) (Policy, bool) {
	now := r.opts.Now()
	key := strings.Join([]string{c.ProjectID, c.Tool, c.Repository.Origin}, "\x00")
	if ttl := r.opts.PolicyCacheTTL; ttl > 0 {
		r.cache.mu.Lock()
		e, hit := r.cache.policies[key]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl {
			return e.pol, e.ok
		}
	}
	pol, err := r.opts.Resolve(c)
	ok := err == nil && pol.Endpoint != "" && pol.Key != ""
	if r.opts.PolicyCacheTTL > 0 {
		r.cache.mu.Lock()
		r.cache.policies[key] = cachedPolicy{pol, ok, now}
		r.cache.mu.Unlock()
	}
	return pol, ok
}

func (lc *lookupCache) expire(now time.Time) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	for k, e := range lc.claims {
		if now.Sub(e.at) > time.Minute {
			delete(lc.claims, k)
		}
	}
	for k, e := range lc.policies {
		if now.Sub(e.at) > time.Minute {
			delete(lc.policies, k)
		}
	}
}
