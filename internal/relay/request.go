package relay

import "fmt"

// request is the next request a route sends: the parts it delivers, their body as the policy
// now lets it leave, and what the policy took. head is set when the first part alone is over
// maxMergeBytes once filtered, as an entry an older relay wrote or one a stricter policy grew
// can be: body is then the part's first piece, and the rest of the part stays queued.
type request struct {
	batch []entry
	body  []byte // nil: the policy withholds all of the first part, or no piece of it fits the host
	tally sendTally
	head  *headSplit
}

// take reads the oldest parts, applies the policy as it stands now to each, and merges them
// while their filtered bodies fit maxMergeBytes, so the bound holds on what is sent. A part
// over the bound once filtered is cut, as splitHead says, rather than sent whole.
func (s *sender) take(entries []entry, pol Policy) (request, error) {
	first := entries[0]
	stored, err := s.r.outbox.read(s.route, first)
	if err != nil {
		// A file that cannot be read never will be; it must not hold the rest back.
		s.unreadable(first)
		return request{}, err
	}
	body, tl := s.r.withholdQueued(first.signal, stored, pol)
	req := request{batch: []entry{first}, body: body, tally: tl}
	if body == nil {
		return req, nil
	}
	if len(body) > maxMergeBytes {
		h, err := splitHead(first.signal, stored, body, maxMergeBytes)
		if err != nil {
			s.unreadable(first)
			return request{}, err
		}
		if h.body != nil {
			// The piece alone, so what the policy took is counted once per record sent, not per piece.
			if h.body, req.tally = s.r.withholdQueued(first.signal, h.stored, pol); h.body == nil {
				return request{}, fmt.Errorf("queued %s piece no longer decodes", first.signal)
			}
		}
		req.body, req.head = h.body, &h
		return req, nil
	}
	bodies, size := [][]byte{body}, len(body)
	for _, e := range entries[1:] {
		if len(req.batch) >= maxMergeFiles || e.signal != first.signal {
			break
		}
		b, err := s.r.outbox.read(s.route, e)
		if err != nil {
			break
		}
		fb, t := s.r.withholdQueued(e.signal, b, pol)
		if fb == nil || size+len(fb) > maxMergeBytes {
			break
		}
		req.batch, bodies, size = append(req.batch, e), append(bodies, fb), size+len(fb)
		req.tally = req.tally.plus(t)
	}
	if len(req.batch) == 1 {
		return req, nil
	}
	merged, err := mergeBodies(first.signal, bodies)
	if err != nil {
		return request{batch: req.batch[:1], body: body, tally: tl}, nil
	}
	req.body = merged
	return req, nil
}

// settleHead ends a head that was delivered, refused, or too large for any request: its
// records no request can carry are dropped, and the part keeps only what is left of it, in its
// place, or leaves the outbox when nothing is. Should the shortened part not be written, the
// part stays whole and its head is sent again, as when a delivered part's removal fails; should
// the janitor drop the part while its head is in flight, the shortened part is written back and
// delivered after all, which overcounts the drop, never loses a record.
func (s *sender) settleHead(e entry, h *headSplit) {
	if h.tooLarge > 0 {
		s.r.stats.dropped(e.signal, "too_large", h.tooLarge)
		s.r.warnf("outbox %s: %d %s cannot fit a %d-byte request beside their resource; dropped", s.route, h.tooLarge, e.signal, maxRequestBytes)
	}
	if h.rest == nil {
		s.r.outbox.remove(s.route, []entry{e})
		s.forget([]entry{e})
		return
	}
	ne, err := s.r.outbox.replace(s.route, e, h.rest, h.restRecords)
	if err != nil {
		s.r.warnf("outbox %s: %v; its part will be sent again from its start", s.route, err)
		return
	}
	s.swap(e, ne)
}

func (s *sender) unreadable(e entry) {
	s.r.stats.dropped(e.signal, "outbox_unreadable", e.records)
	s.r.outbox.bury(s.route, e)
	s.forget([]entry{e})
}
