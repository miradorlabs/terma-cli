package relay

import (
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

const (
	sendTimeout   = 15 * time.Second
	maxMergeBytes = 4 << 20
	maxMergeFiles = 64
	minBackoff    = time.Second
	maxBackoff    = 2 * time.Minute
	maxRetryAfter = 10 * time.Minute
	// A refused key is not fixed by asking again soon: `terma setup` stores a new one.
	refusedBackoff    = 5 * time.Minute
	maxRefusedBackoff = time.Hour
	keylessRetry      = time.Minute
	idleWait          = 30 * time.Second
)

// sender delivers one outbox queue so one refusing host never holds up another destination.
// It runs in one of two modes determined by whether team is nil:
//   - Legacy mode (team == nil): delivers a single route from <dir>/<route>, resolving policy
//     via Relay.resolve with no pre-POST admission fence.
//   - Team mode (team != nil): delivers a routed enrollment queue from <dir>/.teams/<team>,
//     reading per-file Queued headers, rechecking each entry via Router.Recheck, and running a
//     pre-POST admission fence before sending.
type sender struct {
	// Shared across both modes.
	r    *Relay
	dir  string
	name string
	wake chan struct{}

	mu sync.Mutex
	// queue is the outbox in arrival order, one queue per signal, so the sender never
	// lists the directory again after recovery: a backed-up destination drains in linear time.
	queue   map[Signal][]entry
	keyless bool // a waiting route keeps no relay awake

	// Legacy mode only (team == nil).
	route route

	// Team mode only (team != nil).
	team        *teamRoute
	stateChange chan struct{}
	blocked     bool              // guarded by mu; true while team is waiting on state/policy
	stateID     string            // routing state id observed by nextBatch
	gen         int64             // routing state generation observed by nextBatch
	scanAfter   map[Signal]string // cursor for incremental scan across waiting entries
	scanFound   bool              // whether the current scan pass sent at least one batch
	scanWait    bool              // whether the current scan pass saw any waiting entry
}

func (r *Relay) sender(rt route) *sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.senders[rt]
	if s == nil {
		s = &sender{r: r, route: rt, dir: r.outbox.routeDir(rt), name: rt.String(), wake: make(chan struct{}, 1), queue: map[Signal][]entry{}}
		r.senders[rt] = s
		r.wg.Add(1)
		go s.loop()
	}
	return s
}

// enqueue writes a part to its route's outbox before the export is answered, so a crash
// loses none. Its file is named by at, when the part arrived, so a part released from the
// hold sorts before the ones that arrived while it waited.
func (r *Relay) enqueue(c claim.Claim, p *part, at time.Time) {
	rt := routeOf(p.account, c)
	body, err := proto.Marshal(p.msg)
	if err != nil {
		r.stats.dropped(p.signal, "encode_failed", p.records)
		return
	}
	e := newEntry(at, p.signal, p.records)
	if err := r.outbox.put(rt, c.Repository, e, body); err != nil {
		r.stats.dropped(p.signal, "outbox_write_failed", p.records)
		r.warnf("outbox %s: %v; dropped %d %s", rt, err, p.records, p.signal)
		return
	}
	s := r.sender(rt)
	s.push(e)
	s.poke()
}

func (s *sender) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// busy ignores a keyless route, since only a new key, or a sign-in that may send it, can
// move it.
func (s *sender) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queue {
		if len(q) > 0 {
			return !s.keyless
		}
	}
	return false
}

func (s *sender) setKeyless(v bool) {
	s.mu.Lock()
	s.keyless = v
	if s.team != nil {
		s.blocked = v
	}
	s.mu.Unlock()
}

// loop delivers the outbox oldest first; a stopping relay keeps going within its grace.
func (s *sender) loop() {
	defer s.r.wg.Done()
	ctx := s.r.sendCtx
	backoff := time.Duration(0)
	// single sends one file per request after a merged request was refused, so one bad file is set aside alone.
	single := false
	var c claim.Claim
	if s.team == nil {
		c = claim.Claim{ProjectID: s.route.project, Tool: s.route.toolLabel(), Repository: s.r.outbox.identity(s.route), Account: s.route.account}
	}
	stopping := func() bool {
		select {
		case <-s.r.stopping:
			return true
		default:
			return false
		}
	}
	resetScan := func() {
		backoff = 0
		s.scanAfter = nil
		s.scanFound = false
		s.scanWait = false
	}
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		wake := s.wake
		if backoff > 0 {
			wake = nil // new work does not cut a backoff short: the host is the same
		}
		stop := s.r.stopping
		if stopping() {
			stop = nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		case <-wake:
		case <-s.stateChange:
			resetScan()
		case <-stop:
			if backoff > 0 {
				return false // a failing host is not retried on the way out
			}
		}
		return true
	}
	for ctx.Err() == nil {
		select {
		case <-s.stateChange:
			resetScan()
		default:
		}
		limit := maxMergeFiles
		if single {
			limit = 1
		}
		entries, idle := s.nextBatch(limit)
		if len(entries) == 0 {
			if idle == 0 {
				continue
			}
			if stopping() {
				return
			}
			if !sleep(idle) {
				return
			}
			continue
		}
		pol, ok := s.resolveBatchPolicy(c)
		// Another person's or environment's backlog waits for its own sign-in, and ages out;
		// a server key sends only what no person's sign-in collected.
		if !ok {
			s.setKeyless(true)
			if stopping() || !sleep(keylessRetry) {
				return
			}
			continue
		}
		s.setKeyless(false)
		// The repository list as it stands now: a repository removed since sends none still queued.
		if s.team == nil && pol.Unadmitted {
			for _, e := range entries {
				s.r.stats.dropped(e.signal, "policy_repository", e.records)
			}
			s.removeFiles(entries)
			continue
		}
		batch, body, err := s.take(entries)
		if err != nil {
			if !sleep(minBackoff) {
				return
			}
			continue
		}
		// The content policy as it stands now: a project that turned prompts off since sends none still queued.
		// Counted only once the batch leaves the outbox: a retry filters it again.
		body, tl := s.r.filterQueued(batch[0].signal, body, pol, s.team)
		if pol.Signals != nil && !contains(pol.Signals, string(batch[0].signal)) || body == nil {
			reason, records := "policy_signal_or_content", 0
			for _, e := range batch {
				records += e.records
			}
			s.r.stats.dropped(batch[0].signal, reason, records)
			s.removeFiles(batch)
			continue
		}
		out, wait, detail, rejected := s.postBatch(ctx, pol, batch, body)
		switch out {
		case sent:
			s.r.count(batch[0].signal, tl)
			records := 0
			for _, e := range batch {
				records += e.records
			}
			s.r.stats.forwarded(batch[0].signal, records)
			if rejected > 0 {
				// Resending a partial success would only duplicate what was kept.
				s.r.stats.add("upstream_rejected."+string(batch[0].signal), int(rejected))
			}
			s.removeFiles(batch)
			single, backoff = false, 0
		case aborted:
			continue
		case refused:
			if len(batch) > 1 {
				single = true
				continue
			}
			single = false
			s.r.stats.dropped(batch[0].signal, "upstream_"+detail, batch[0].records)
			s.r.warnf("upstream %s refused %d %s: %s", s.name, batch[0].records, batch[0].signal, detail)
			s.buryFile(batch[0])
		case keyless:
			// Waiting for a sign-in, as a route no sign-in may send is: idle, not retrying.
			s.setKeyless(true)
			backoff = 0 // no host is at fault: new parts may wake it
			if strings.HasPrefix(detail, signInErrPrefix) {
				s.r.warnf("relay %s: %s; waiting for a sign-in", s.name, detail)
			} else if s.r.opts.Logf != nil {
				s.r.opts.Logf("relay %s: %s; waiting", s.name, detail)
			}
			if stopping() || !sleep(keylessRetry) {
				return
			}
		case retry:
			if ctx.Err() != nil {
				return
			}
			s.r.stats.add("upstream_retries", 1)
			s.r.warnf("upstream %s: %s; retrying", s.name, detail)
			backoff = nextBackoff(backoff, wait)
			if !sleep(jitter(backoff, wait)) {
				return
			}
		}
	}
}

func (s *sender) take(entries []entry) ([]entry, []byte, error) {
	first := entries[0]
	body, err := s.readFile(first)
	if err != nil {
		// A file that cannot be read never will be; it must not hold the rest back.
		s.r.stats.dropped(first.signal, "outbox_unreadable", first.records)
		s.buryFile(first)
		return nil, nil, err
	}
	batch := []entry{first}
	bodies := [][]byte{body}
	size := len(body)
	for _, e := range entries[1:] {
		if len(batch) >= maxMergeFiles || e.signal != first.signal {
			break
		}
		b, err := s.readFile(e)
		if err != nil || size+len(b) > maxMergeBytes {
			break
		}
		batch = append(batch, e)
		bodies = append(bodies, b)
		size += len(b)
	}
	if len(batch) == 1 {
		return batch, body, nil
	}
	merged, err := mergeBodies(first.signal, bodies)
	if err != nil {
		return batch[:1], body, nil
	}
	return batch, merged, nil
}

type outcome int

const (
	sent outcome = iota
	retry
	refused
	// keyless is a send no sign-in may make now (retired, another person's, or one whose
	// token can't be had): the route waits as one with no policy does, no host at fault.
	keyless
	// aborted is stopped by the pre-POST admission fence: re-evaluated without backoff.
	aborted
)
