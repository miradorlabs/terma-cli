package relay

import (
	"fmt"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// recordFraming bounds the tag and length prefix protobuf puts before one nested message.
	recordFraming = 6
	// maxRequestBytes is the largest body the OTLP host accepts; it refuses a larger one.
	maxRequestBytes = 4 << 20
	// maxRepeatedBytes is what a header-heavy block may take in requests when that is over three
	// times its size: 16 full requests, a quarter of the outbox. Past it the block is dropped, as
	// an export that repeats a resource near the host's limit once per record would otherwise
	// fill the outbox and the relay's memory with copies of it.
	maxRepeatedBytes = 64 << 20
)

// run is records [from, to) of block b: one resource and scope's records.
type run struct{ block, from, to int }

// cutPlan is a queued part laid out for the host: its pieces as queued, in order, with their
// records, and the records it drops.
type cutPlan struct {
	bodies  [][]byte
	records []int
	dropped unsendable
}

// unsendable counts the records packRuns drops: tooLarge cannot fit the host's limit beside
// their resource, repeated belong to a header-heavy block whose requests would pass
// maxRepeatedBytes and three times its size.
type unsendable struct{ tooLarge, repeated int }

// rejects are the runs packRuns drops, by reason.
type rejects struct{ tooLarge, repeated []run }

// within divides a part whose encoding is over prefer into parts for the host, in order, as
// packRuns lays them out, and counts the records packRuns drops; the caller drops them.
func (p *part) within(prefer int) (parts []*part, dropped unsendable) {
	if proto.Size(p.msg) <= prefer {
		return []*part{p}, unsendable{}
	}
	l, ok := layoutOf(p.msg)
	if !ok {
		return []*part{p}, unsendable{}
	}
	msgs, out := packRuns(prefer, maxRequestBytes, l.heads, l.sizes, l.alone)
	for _, runs := range msgs {
		q := *p
		q.msg, q.records = l.build(runs), l.count(runs)
		parts = append(parts, &q)
	}
	return parts, unsendable{l.count(out.tooLarge), l.count(out.repeated)}
}

// planCut lays a queued part whose filtered form is over prefer out as packRuns does, on its
// filtered sizes, and cuts the part as queued along the same lines. Withholding changes records
// in place and never drops or reorders them, so the queued and filtered bodies share their
// blocks; when they do not, the pieces are kept filtered, which sends nothing a stricter policy
// would withhold.
func planCut(sig Signal, stored, filtered []byte, prefer int) (cutPlan, error) {
	sm, fm := newSignalMessage(sig), newSignalMessage(sig)
	if err := proto.Unmarshal(stored, sm); err != nil {
		return cutPlan{}, fmt.Errorf("decode queued %s: %w", sig, err)
	}
	if err := proto.Unmarshal(filtered, fm); err != nil {
		return cutPlan{}, fmt.Errorf("decode filtered %s: %w", sig, err)
	}
	lf, _ := layoutOf(fm)
	ls, _ := layoutOf(sm)
	if len(ls.sizes) != len(lf.sizes) {
		ls = lf
	}
	for b := range ls.sizes {
		if len(ls.sizes[b]) != len(lf.sizes[b]) {
			ls = lf
			break
		}
	}
	msgs, out := packRuns(prefer, maxRequestBytes, lf.heads, lf.sizes, lf.alone)
	p := cutPlan{dropped: unsendable{lf.count(out.tooLarge), lf.count(out.repeated)}}
	for _, runs := range msgs {
		body, err := proto.Marshal(ls.build(runs))
		if err != nil {
			return cutPlan{}, err
		}
		p.bodies, p.records = append(p.bodies, body), append(p.records, ls.count(runs))
	}
	return p, nil
}

func newSignalMessage(sig Signal) proto.Message {
	switch sig {
	case Logs:
		return &logspb.LogsData{}
	case Traces:
		return &tracepb.TracesData{}
	default:
		return &metricspb.MetricsData{}
	}
}

// packRuns lays every block's records out, in order, into messages for a host that refuses one
// over hard, preferring messages within prefer. heads[b] is block b's framed resource and scope
// and sizes[b] its framed records; a message repeats the header of every block it carries.
//
//   - A block whose header is at most half of prefer packs to prefer, sharing messages with its
//     neighbours. Two consecutive messages of the block then carry more of its record bytes than
//     one header, so its share of the messages is at most three times its own size.
//   - A block with a larger header packs alone, each message filled to hard before the next
//     opens, so it repeats the header as few times as an in-order cut can. A header near hard
//     leaves room for few records per message, so the copies can pass three times the block:
//     they are kept up to maxRepeatedBytes, and past both bounds the block is dropped as
//     repeated (TestPartWithinHeaderHeavyExports measures both).
//   - A record too large to share a message goes alone; one whose message alone, exactly as
//     encoded (alone), is over hard is dropped as too large.
func packRuns(prefer, hard int, heads []int, sizes [][]int, alone func(b, i int) int) (msgs [][]run, out rejects) {
	size, sealed := 0, true // sealed: the next record opens a message
	for b, recs := range sizes {
		h := heads[b]
		if 2*h > prefer {
			own, tooLarge, repeated := packAlone(b, hard, h, recs, alone)
			msgs = append(msgs, own...)
			out.tooLarge, out.repeated = append(out.tooLarge, tooLarge...), append(out.repeated, repeated...)
			sealed = true
			continue
		}
		open := false
		for i, n := range recs {
			if h+n > hard && alone(b, i) > hard {
				out.tooLarge = append(out.tooLarge, run{block: b, from: i, to: i + 1})
				open = false
				continue
			}
			need := n
			if !open {
				need += h
			}
			if sealed || size+need > prefer {
				msgs = append(msgs, nil)
				size, sealed, open, need = 0, false, false, n+h
			}
			cur := &msgs[len(msgs)-1]
			if !open {
				*cur = append(*cur, run{block: b, from: i})
				open = true
			}
			(*cur)[len(*cur)-1].to = i + 1
			size += need
		}
	}
	return msgs, out
}

// packAlone packs a header-heavy block b in messages of its own, each filled up to hard. It
// drops a record that cannot fit hard beside the header as too large, and the block's other
// records as repeated when the messages would total over both three times the block and
// maxRepeatedBytes.
func packAlone(b, hard, h int, recs []int, alone func(b, i int) int) (msgs [][]run, tooLarge, repeated []run) {
	size, total, kept := 0, 0, 0
	for i, n := range recs {
		if h+n > hard && alone(b, i) > hard {
			tooLarge = append(tooLarge, run{block: b, from: i, to: i + 1})
			size = 0
			continue
		}
		if size == 0 || size+n > hard {
			msgs = append(msgs, []run{{block: b, from: i}})
			size, total = h, total+h
		}
		msgs[len(msgs)-1][0].to = i + 1
		size, total, kept = size+n, total+n, kept+n
	}
	if total > max(3*(h+kept), maxRepeatedBytes) {
		for _, m := range msgs {
			repeated = append(repeated, m...)
		}
		return nil, tooLarge, repeated
	}
	return msgs, tooLarge, nil
}
