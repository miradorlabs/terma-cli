package relay

import (
	"slices"
	"strings"
)

// push queues a part enqueue wrote; enqueue runs under deliverMu, so each queue stays in arrival order.
func (s *sender) push(e entry) {
	s.mu.Lock()
	s.queue[e.signal] = append(s.queue[e.signal], e)
	s.mu.Unlock()
}

// recover queues parts a previous relay left in the outbox, in arrival order with any queued
// already, and returns the ones it added: a part already queued is not queued twice.
func (s *sender) recover(entries []entry) []entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	known := map[string]bool{}
	for _, q := range s.queue {
		for _, e := range q {
			known[e.name] = true
		}
	}
	var added []entry
	for _, e := range entries {
		if !known[e.name] {
			s.queue[e.signal] = append(s.queue[e.signal], e)
			added = append(added, e)
		}
	}
	for sig := range s.queue {
		slices.SortFunc(s.queue[sig], func(a, b entry) int { return strings.Compare(a.name, b.name) })
	}
	return added
}

// next is the next batch to send: up to limit parts of the signal whose oldest part is oldest of
// all, in arrival order. Each signal goes to its own endpoint, so merging a signal's parts
// across the others keeps every order a host can see.
func (s *sender) next(limit int) []entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var head []entry
	for _, q := range s.queue {
		if len(q) > 0 && (head == nil || q[0].name < head[0].name) {
			head = q
		}
	}
	return slices.Clone(head[:min(limit, len(head))])
}

// forget takes parts that left the outbox, delivered, dropped or buried, off the queue.
func (s *sender) forget(batch []entry) {
	if len(batch) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := make(map[string]bool, len(batch))
	for _, e := range batch {
		gone[e.name] = true
	}
	for sig, q := range s.queue {
		// The usual case: the batch is the head of its signal's queue.
		n := 0
		for n < len(q) && gone[q[n].name] {
			n++
		}
		q = q[n:]
		if n < len(batch) {
			q = slices.DeleteFunc(q, func(e entry) bool { return gone[e.name] })
		}
		if len(q) == 0 {
			q = nil // let a drained queue's array go
		}
		s.queue[sig] = q
	}
}
