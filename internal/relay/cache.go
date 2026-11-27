package relay

import (
	"slices"
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
	// reread are the processes this claim was read again for (lookupFor), not named in it.
	reread []int
}

type cachedPolicy struct {
	pol Policy
	ok  bool
	at  time.Time
}

// lookupFor is lookup for a part of pid stamped at: the claim, and its placement covering pid
// then. A cached claim that does not place pid may have been read before pid claimed the
// session (a resumed process's session-start), so it is read again, once per process until
// the cache's moment passes: left unplaced, the part would carry no run, and a sweep of the
// paused run before it would take it.
func (r *Relay) lookupFor(session string, pid int, at time.Time) (claim.Claim, bool, claim.Claim, bool) {
	now := r.opts.Now()
	if ttl := r.opts.ClaimCacheTTL; ttl > 0 {
		r.cache.mu.Lock()
		e, hit := r.cache.claims[session]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl {
			if c, placed := e.c.At(pid, at); e.ok && placed || slices.Contains(e.reread, pid) {
				return e.c, e.ok, c, placed
			}
			raw, ok := r.opts.Lookup(session, now)
			r.cache.mu.Lock()
			r.cache.claims[session] = cachedClaim{c: raw, ok: ok, at: e.at, reread: append(slices.Clone(e.reread), pid)}
			r.cache.mu.Unlock()
			c, placed := raw.At(pid, at)
			return raw, ok, c, placed
		}
	}
	raw, ok := r.lookup(session)
	c, placed := raw.At(pid, at)
	return raw, ok, c, placed
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
		r.cache.claims[session] = cachedClaim{c: c, ok: ok, at: now}
		r.cache.mu.Unlock()
	}
	return c, ok
}

// resolve is c's policy for a part in lane, the sign-in it leaves under. Its answer is kept
// per lane, and one resolved under another person's sign-in (Policy.Account) is never
// served: within the cache's moment a switch would otherwise send B's part, with B's token,
// to the team A's profile chose.
func (r *Relay) resolve(c claim.Claim, lane string) (Policy, bool) {
	now := r.opts.Now()
	key := strings.Join([]string{lane, c.ProjectID, c.Tool, c.Repository.Origin}, "\x00")
	if ttl := r.opts.PolicyCacheTTL; ttl > 0 {
		r.cache.mu.Lock()
		e, hit := r.cache.policies[key]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl && (!e.ok || e.pol.resolvedFor(lane)) {
			return e.pol, e.ok
		}
	}
	pol, err := r.opts.Resolve(c)
	ok := err == nil && pol.Endpoint != "" && (pol.Key != "" || r.opts.SignIn != nil) && pol.resolvedFor(lane)
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
