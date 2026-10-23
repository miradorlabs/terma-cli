package relay

import (
	"errors"
	"fmt"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// errCut reports that take cut the oldest part into pieces in its place: the pieces are next.
var errCut = errors.New("the oldest part was cut")

// request is the next request a route sends: the parts it delivers, their body as the policy
// now lets it leave, and what the policy took.
type request struct {
	batch []entry
	body  []byte // nil: the policy withholds all of the first part
	tally sendTally
}

// take reads the oldest parts, applies the policy as it stands now to each, and merges them
// while their filtered bodies fit maxMergeBytes, so the bound holds on what is sent. A part
// over the bound once filtered, as an older relay wrote or a stricter policy grew, is cut into
// parts that fit it, in its place (errCut), unless it is one message the host takes whole.
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
		p, err := planCut(first.signal, stored, body, maxMergeBytes)
		if err != nil {
			s.unreadable(first)
			return request{}, err
		}
		if len(p.bodies) == 1 && p.dropped == (unsendable{}) {
			return req, nil // one record, or one resource, too large for maxMergeBytes: within maxRequestBytes
		}
		if err := s.cut(first, p); err != nil {
			return request{}, err
		}
		return request{}, errCut
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

// cut puts p's pieces in e's place, on disk and on the queue, and drops the records no request
// can carry. It runs under deliverMu, as the janitor's drops do, and only while e is still
// queued, so a part the janitor dropped meanwhile stays dropped.
func (s *sender) cut(e entry, p cutPlan) error {
	s.r.deliverMu.Lock()
	defer s.r.deliverMu.Unlock()
	if !s.queued(e) {
		return fmt.Errorf("outbox %s: %s left the queue before it was cut", s.route, e.name)
	}
	pieces, err := s.r.outbox.split(s.route, e, p.bodies, p.records)
	if err != nil {
		s.r.warnf("outbox %s: %v; its part will be cut again", s.route, err)
		return err
	}
	s.r.dropUnsendable("outbox "+s.route.String(), e.signal, p.dropped)
	s.replace(e, pieces)
	return nil
}

func (s *sender) unreadable(e entry) {
	s.r.stats.dropped(e.signal, "outbox_unreadable", e.records)
	s.r.outbox.bury(s.route, e)
	s.forget([]entry{e})
}

func mergeBodies(sig Signal, bodies [][]byte) ([]byte, error) {
	switch sig {
	case Logs:
		var out logspb.LogsData
		for _, b := range bodies {
			var m logspb.LogsData
			if err := proto.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			out.ResourceLogs = append(out.ResourceLogs, m.ResourceLogs...)
		}
		return proto.Marshal(&out)
	case Traces:
		var out tracepb.TracesData
		for _, b := range bodies {
			var m tracepb.TracesData
			if err := proto.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			out.ResourceSpans = append(out.ResourceSpans, m.ResourceSpans...)
		}
		return proto.Marshal(&out)
	default:
		var out metricspb.MetricsData
		for _, b := range bodies {
			var m metricspb.MetricsData
			if err := proto.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			out.ResourceMetrics = append(out.ResourceMetrics, m.ResourceMetrics...)
		}
		return proto.Marshal(&out)
	}
}

// OutboxDir is the outbox's directory name under the relay directory.
const OutboxDir = "outbox"
