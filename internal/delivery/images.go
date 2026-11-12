package delivery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A terma.prompt.image record leaves in a lane of its own: hooks spool it to the image
// spool (spool.ImageDir), and the relay's image sender (ImageSender) sends it, so no event
// waits on an upload and no upload on a deadline of a pass. The record holds no bytes; they
// wait in the image store (promptimage) and join the record only in its own request, and the
// store forgets them once no queued record names them.

// imageMaxAttempts is how many sends of an image that reached the host and did not get
// through it is given before its record is sent without its bytes (unsent).
const imageMaxAttempts = 4

// ImageResult summarizes one pass over the image queue; its counters are disjoint, one per
// reason, and count image records.
type ImageResult struct {
	Sent, Held, Failed, Expired, Pruned, Dropped, Unroutable, Withheld int
	// Deferred counts the images left untried: their project's window was open, or the
	// sender stopped first.
	Deferred int
	// Rejected counts the records dropped because the host refused them even without their
	// bytes, more than imageMaxAttempts times.
	Rejected int
	// Unread says the queue could not be read: the pass did nothing else.
	Unread bool
	Err    error
	// Failures names each project whose image the host refused, left unanswered or could not
	// be reached for; Stalls each whose upload did not finish in its time (the uplink, which
	// fails nothing). Both are requeued and back off the project's images.
	Failures, Stalls []Failure
	// Waiting names each project whose images window was open.
	Waiting   []Wait
	Endpoints []string

	// read is the queue's Stamp as the pass read it, and first the project it started with:
	// what the relay's sender carries to its next pass.
	read  spool.Stamp
	first string
}

// nextWindow is the earliest end of the windows the pass waited out (Waiting) or opened
// (Failures, Stalls; one cut short by the sender stopping has none), zero for none. Every
// window a pass learns of is named in one of them, and read here alone.
func (res ImageResult) nextWindow() time.Time {
	var next time.Time
	ends := func(at time.Time) {
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	for _, w := range res.Waiting {
		ends(w.RetryAt)
	}
	for _, f := range slices.Concat(res.Failures, res.Stalls) {
		ends(f.RetryAt)
	}
	return next
}

// isImage reports whether e is a terma.prompt.image record.
func isImage(e spool.Event) bool { return e.Name == semconv.TermaPromptImageEvent }

// splitImages separates the image records from the other events, keeping each one's order.
func splitImages(events []spool.Event) (others, images []spool.Event) {
	for _, e := range events {
		if isImage(e) {
			images = append(images, e)
		} else {
			others = append(others, e)
		}
	}
	return others, images
}

// imageID is an image record's image id, "" for none.
func imageID(e spool.Event) string {
	id, _ := e.Attrs[semconv.TermaImageIDKey].(string)
	return id
}

// imageQueue is one project's images in a pass, the next first.
type imageQueue struct {
	id     string
	sender *otlpSender
	items  []imageItem
}

// imageItem is a queued image record and the record as it leaves.
type imageItem struct {
	queued spool.Pending
	out    spool.Event
}

// FlushImages delivers the image queue, r.Images, once: each image in a request of its own,
// taken from the queue before it is sent (so it never goes alongside the relay's sender) and
// settled as soon as it went. `terma spool flush --images` runs it in the foreground; the
// relay's ImageSender runs it whenever it is woken. events is the events' spool, whose
// records from an older build may still name stored bytes; nil keeps every stored image.
func (r Router) FlushImages(ctx context.Context, events *spool.Spool, cfg *config.Config, force bool) ImageResult {
	return r.flushImages(ctx, events, cfg, force, imageTurns{prune: true})
}

// imageTurns is how the relay's sender runs a pass: it starts after the project after
// names, so the one that went first last time goes last (ImageResult.first says which went
// first this time); gate, set, is asked before each image and told when it is done; now,
// set, is the clock the pass judges and opens windows by.
type imageTurns struct {
	after string
	gate  imageGate
	now   func() time.Time
	// prune has the pass prune the image store of what outlived every record.
	prune bool
}

// imageGate is the sender's say over each image a pass sends: begin is asked first, and a
// no stops the pass (the sender halted); end is told once the image is done.
type imageGate interface {
	begin() bool
	end()
}

// flushImages is FlushImages, with turns.
func (r Router) flushImages(ctx context.Context, events *spool.Spool, cfg *config.Config, force bool, turns imageTurns) ImageResult {
	s := r.Images
	if s == nil {
		return ImageResult{Err: errNoImageLane}
	}
	clock := turns.now
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	p := &imagePass{s: s, accepted: map[string]bool{}, now: clock}
	res := &p.res
	queued, stamp, read, err := s.Read(ctx, now)
	res.Expired, res.Pruned, res.Dropped = read.Expired, read.Pruned, read.Dropped
	res.read = stamp
	if err != nil {
		res.Err, res.Unread = err, true
		return *res
	}
	byProject := map[string][]imageItem{}
	places := r.places(cfg)
	var gone []spool.Pending
	for _, q := range queued {
		id, out, place := places.of(ctx, q.Event)
		switch place {
		case placeSend:
			byProject[id] = append(byProject[id], imageItem{queued: q, out: out})
			continue
		case placeHeld:
			res.Held++
			continue
		case placeUnroutable:
			res.Unroutable++
		case placeWithheld:
			res.Withheld++
		}
		gone = append(gone, q)
		p.ids = append(p.ids, imageID(q.Event))
	}
	// What never leaves goes in one rewrite; a delivered image is settled on its own.
	if err := s.Remove(gone...); err != nil && !errors.Is(err, spool.ErrNotQueued) {
		p.errs = append(p.errs, fmt.Errorf("remove image records that cannot leave: %w", err))
	}
	var queues []*imageQueue
	for _, id := range startingAfter(slices.Sorted(maps.Keys(byProject)), turns.after) {
		items := byProject[id]
		// The window first: a project waiting out one needs no key this pass.
		if next := s.RetryAt(id); !force && now.Before(next) {
			res.Waiting = append(res.Waiting, Wait{ProjectID: id, RetryAt: next})
			res.Deferred += len(items)
			continue
		}
		sender, err := r.senderFor(cfg, id)
		if err != nil {
			// The keychain is locked or out of reach: as for the events, a failure, with a
			// window doctor shows, asked again when it ends.
			p.errs = append(p.errs, fmt.Errorf("%s: %w", id, err))
			res.Failures = append(res.Failures, Failure{ProjectID: id, Err: err, RetryAt: s.DestinationFailed(id, clock())})
			res.Failed += len(items)
			continue
		}
		if sender == nil {
			res.Held += len(items)
			continue
		}
		slices.SortStableFunc(items, func(a, b imageItem) int {
			return cmp.Or(cmp.Compare(a.out.Attempts, b.out.Attempts), a.out.Time.Compare(b.out.Time))
		})
		queues = append(queues, &imageQueue{id: id, sender: sender, items: items})
	}
	if len(queues) > 0 {
		res.first = queues[0].id
	}
	for len(queues) > 0 {
		var next []*imageQueue
		for _, q := range queues {
			if r.SetUp != nil && !r.SetUp() || ctx.Err() != nil {
				// Torn down, or the sender is stopping: what is left stays queued.
				for _, q := range queues {
					res.Deferred += len(q.items)
				}
				return p.finish(r, events, now, turns.prune)
			}
			item := q.items[0]
			q.items = q.items[1:]
			sent, halted := p.send(ctx, r, q, item, turns.gate)
			if halted {
				// The sender was halted: this image and what is left stay queued.
				res.Deferred++
				for _, q := range queues {
					res.Deferred += len(q.items)
				}
				return p.finish(r, events, now, turns.prune)
			}
			if !sent {
				// The project's other images wait for the window just opened.
				res.Deferred += len(q.items)
				q.items = nil
			}
			if len(q.items) > 0 {
				next = append(next, q)
			}
		}
		queues = next
	}
	return p.finish(r, events, now, turns.prune)
}

// imagePass is what one pass over the image queue has done so far.
type imagePass struct {
	s *spool.Queue
	// now is the pass's clock, for the windows it judges and opens.
	now      func() time.Time
	res      ImageResult
	errs     []error
	accepted map[string]bool
	// ids are the images this pass sent or let go; their bytes stay until no waiting
	// record names them.
	ids []string
}

// send takes item from the queue, then asks gate to begin, sends it and records how that
// went, unless another sender settled it meanwhile; sent is false when the project's other
// images wait, halted true when gate said no. The take comes first: a sender waiting for the
// queue behind another (a manual flush) is not uploading, and may be halted meanwhile.
func (p *imagePass) send(ctx context.Context, r Router, q *imageQueue, item imageItem, gate imageGate) (sent, halted bool) {
	taken, err := p.s.Take(ctx, item.queued)
	switch {
	case errors.Is(err, spool.ErrNotQueued):
		return true, false
	case err != nil && ctx.Err() != nil:
		// The sender stopped while another sender held the queue.
		p.res.Deferred++
		return true, false
	case err != nil:
		// The queue's lock cannot be taken: a failure here, not the host's, with a window,
		// so the image is tried again when it ends rather than at once.
		p.res.Failures = append(p.res.Failures, Failure{ProjectID: q.id, Err: err, RetryAt: p.s.DestinationFailed(q.id, p.now())})
		p.errs = append(p.errs, fmt.Errorf("take an image record: %w", err))
		p.res.Failed++
		return false, false
	}
	defer taken.Release()
	if gate != nil {
		if !gate.begin() {
			return false, true
		}
		defer gate.end()
	}
	return p.record(ctx, q, item, taken, r.sendImage(ctx, q.sender, item.out)), false
}

// finish compacts what the pass settled out of the queue, in one rewrite, sweeps the bytes
// it let go and sums it up.
func (p *imagePass) finish(r Router, events *spool.Spool, start time.Time, prune bool) ImageResult {
	read, err := p.s.Compact(p.res.read)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("compact the image queue: %w", err))
	}
	// The compaction is no news to the sender: only what came since is.
	p.res.read = read
	r.forgetImages(p.ids, start, events, p.s, prune)
	p.res.Endpoints = slices.Sorted(maps.Keys(p.accepted))
	p.res.Err = errors.Join(append([]error{p.res.Err}, p.errs...)...)
	return p.res
}

// startingAfter is ids, sorted, rotated to start with the first after mark, so the
// project that went first last pass goes last this one.
func startingAfter(ids []string, mark string) []string {
	i, _ := slices.BinarySearch(ids, mark)
	if i < len(ids) && ids[i] == mark {
		i++
	}
	i %= max(len(ids), 1)
	return slices.Concat(ids[i:], ids[:i])
}

// forgetImages removes the stored bytes of the images ids this pass sent or let go that no
// record still queued in spools names, and prunes what outlived the spools' records. It
// sweeps nothing unless it read every spool: a nil one may hold records it cannot see. The
// events' spool goes first: its pass hands an image on to the lane before letting it go, so
// a record moving meanwhile is found in one or the other.
//
// A record parked in a repository's git directory (hookrun's park) is in no spool: should
// its image's other record go first, it later leaves saying its bytes are unavailable.
// Parking is a sandboxed hook's last resort and such twins rare, so that is accepted.
func (r Router) forgetImages(ids []string, passStart time.Time, events *spool.Spool, images *spool.Queue, prune bool) {
	if prune {
		promptimage.Prune(r.StateDir, passStart.Add(-spool.MaxAge))
	}
	if len(ids) == 0 || events == nil {
		return
	}
	named, err := events.Mentions(ids)
	if err != nil {
		return
	}
	found, err := images.Mentions(ids)
	if err != nil {
		return
	}
	maps.Copy(named, found)
	promptimage.Sweep(r.StateDir, ids, func(id string) bool { return named[id] }, passStart)
}
