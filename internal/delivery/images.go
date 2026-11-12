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
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A terma.prompt.image record leaves in a lane of its own: hooks spool it to the image
// spool (spool.ImageDir) and a detached pass sends it, so no event waits on an upload. The
// record holds no bytes; they wait in the image store (promptimage) and join the record only
// in its own request, and the store forgets them once no queued record names them.

// ImagePassTimeout bounds a pass of the image lane. The largest image's request (about
// 2.8 MB) gets timeoutFor's minute at 64 KiB/s and starts only with all of it left: five fit
// in a pass, none in the events' 30 s.
const ImagePassTimeout = 5 * time.Minute

// imageMaxAttempts is how many sends of an image that reached the host and did not get
// through it is given before its record is sent without its bytes (unsent).
const imageMaxAttempts = 4

// imageEnvelope bounds an image request's size besides the image itself.
const imageEnvelope = 8 << 10

// errNoTimeLeft is an image not sent because too little of the pass was left for it.
var errNoTimeLeft = errors.New("too little of the pass left for the image")

// ImageResult summarizes one pass of the image lane; its counters are disjoint, one per
// reason, and count image records.
type ImageResult struct {
	Sent, Held, Failed, Expired, Pruned, Dropped, Unroutable, Withheld int
	// Deferred counts the images left untried for a later pass: a window was open, too
	// little of the pass was left, or the pass ended first.
	Deferred int
	// Rejected counts the records dropped because the host refused them even without their
	// bytes, more than imageMaxAttempts times.
	Rejected int
	Skipped  bool
	Reason   spool.SkipReason
	Err      error
	// Failures names each project whose image the host refused, left unanswered or could not
	// be reached for; Stalls each whose upload did not finish in its time (the uplink, which
	// fails nothing). Both are requeued and back off the project's images.
	Failures, Stalls []Failure
	// Waiting names each project whose images window was open.
	Waiting   []Wait
	Endpoints []string
	// Grew says images were spooled as the pass ended, which it did not see: another pass
	// has work.
	Grew bool
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

// FlushImages delivers the image lane, r.Images, one image to a request, settling each in
// the queue as it goes so a pass cut short resends none. Projects take turns, one image
// each, starting a project further on each pass; a project's images go fewest attempts
// first, then oldest. Only one pass runs at a time. events is the events' spool, whose
// records from an older build may still name stored bytes; nil keeps every stored image.
func (r Router) FlushImages(ctx context.Context, events *spool.Spool, cfg *config.Config, force bool) ImageResult {
	s := r.Images
	if s == nil {
		return ImageResult{Err: errNoImageLane}
	}
	now := time.Now()
	p := &imagePass{s: s, accepted: map[string]bool{}}
	res := &p.res
	fr := s.Drain(ctx, spool.DrainOptions{Timeout: cmp.Or(r.imagePass, ImagePassTimeout), NoWait: true, Now: now},
		func(ctx context.Context, queued []spool.Pending) {
			// Each round of the pass sees the windows as they are now.
			now := time.Now()
			byProject := map[string][]imageItem{}
			places := r.places(cfg)
			held := map[string]bool{}
			var gone []spool.Pending
			for _, q := range queued {
				id, out, place := places.of(ctx, q.Event)
				switch place {
				case placeSend:
					byProject[id] = append(byProject[id], imageItem{queued: q, out: out})
					continue
				case placeHeld:
					res.Held++
					held[id] = true
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
			if err := s.Remove(gone...); err != nil {
				p.errs = append(p.errs, fmt.Errorf("remove image records that cannot leave: %w", err))
			}
			var turns []*imageQueue
			for _, id := range startingAfter(slices.Sorted(maps.Keys(byProject)), s.Mark()) {
				items := byProject[id]
				// The window first: a project waiting out one needs no key this pass.
				if next := s.RetryAt(id); !force && now.Before(next) {
					p.wait(id, next)
					res.Deferred += len(items)
					continue
				}
				sender, err := r.senderFor(cfg, id)
				if err != nil {
					// The keychain is locked or out of reach: as for the events, a failure,
					// asked again by the next pass.
					p.errs = append(p.errs, fmt.Errorf("%s: %w", id, err))
					res.Failed += len(items)
					held[id] = true
					continue
				}
				if sender == nil {
					res.Held += len(items)
					held[id] = true
					continue
				}
				s.Unhold(id)
				slices.SortStableFunc(items, func(a, b imageItem) int {
					return cmp.Or(cmp.Compare(a.out.Attempts, b.out.Attempts), a.out.Time.Compare(b.out.Time))
				})
				turns = append(turns, &imageQueue{id: id, sender: sender, items: items})
			}
			// A project held starts no pass for a while (ImagesDue); a pass still looks.
			s.Hold(now.Add(imageHeldWait), slices.Sorted(maps.Keys(held))...)
			if len(turns) > 0 {
				s.SetMark(turns[0].id)
			}
			for len(turns) > 0 {
				var next []*imageQueue
				for _, q := range turns {
					if r.SetUp != nil && !r.SetUp() {
						// Torn down: what is left is teardown's to remove.
						for _, q := range turns {
							res.Deferred += len(q.items)
						}
						return
					}
					item := q.items[0]
					q.items = q.items[1:]
					if !p.record(ctx, q, item, r.sendImage(ctx, q.sender, item.out)) {
						// The project's other images wait for the window just opened.
						res.Deferred += len(q.items)
						q.items = nil
					}
					if len(q.items) > 0 {
						next = append(next, q)
					}
				}
				turns = next
			}
		})
	if !fr.Skipped {
		r.forgetImages(p.ids, now, events, s)
	}
	res.Endpoints = slices.Sorted(maps.Keys(p.accepted))
	res.Expired = fr.Expired
	res.Pruned = fr.Pruned
	res.Dropped = fr.Dropped
	res.Skipped = fr.Skipped
	res.Reason = fr.Reason
	res.Grew = fr.Grew
	res.Err = errors.Join(append([]error{fr.Err}, p.errs...)...)
	return *res
}

// imageHeldWait is how long a project held for what it needs (a key, a policy, the
// keychain) starts no pass of its own, so the events' flush does not start one each time.
const imageHeldWait = 5 * time.Minute

// imagePass is what one pass of the image lane has done so far.
type imagePass struct {
	s        *spool.Spool
	res      ImageResult
	errs     []error
	accepted map[string]bool
	// ids are the images this pass sent or let go; their bytes stay until no waiting
	// record names them.
	ids []string
}

// settle takes q out of the queue, appending requeue at its tail.
func (p *imagePass) settle(q spool.Pending, requeue ...spool.Event) {
	if err := p.s.Settle(q, requeue...); err != nil {
		p.errs = append(p.errs, fmt.Errorf("settle an image record: %w", err))
	}
}

// reported reports whether the pass has already named project id as waiting, failed or
// stalled: a project is named once, however many rounds see it.
func (p *imagePass) reported(id string) bool {
	has := func(f Failure) bool { return f.ProjectID == id }
	return slices.ContainsFunc(p.res.Waiting, func(w Wait) bool { return w.ProjectID == id }) ||
		slices.ContainsFunc(p.res.Failures, has) || slices.ContainsFunc(p.res.Stalls, has)
}

// wait names project id as waiting out its window until next, once.
func (p *imagePass) wait(id string, next time.Time) {
	if !p.reported(id) {
		p.res.Waiting = append(p.res.Waiting, Wait{ProjectID: id, RetryAt: next})
	}
}

// imageWindow is what an outcome does to its project's images window.
type imageWindow int

const (
	// keepWindow leaves the window as it is; the project's next image goes on.
	keepWindow imageWindow = iota
	// closeWindow closes it: the host took an image.
	closeWindow
	// stallWindow and failWindow open a stall's or a failure's (doctor words them apart);
	// the project's other images wait for it.
	stallWindow
	failWindow
)

// imageRule is what an outcome does with its record and its project.
type imageRule struct {
	// settle takes the record out of the queue: it went.
	settle bool
	// attempt counts an attempt and requeues the record at the queue's tail.
	attempt bool
	window  imageWindow
	// counter is the ImageResult count the record adds to; a stall is named in Stalls.
	counter func(*ImageResult) *int
}

// imageRules is what each of judgeImage's outcomes does:
//
//	outcome       settle  attempt  window  counted in
//	delivered     yes     -        close   Sent
//	deferred      -       -        keep    Deferred
//	stalled       -       yes      stall   Stalls
//	failed        -       yes      fail    Failures, Failed (Rejected once dropped)
//	unavailable   -       -        fail    Failures, Failed
//
// A record whose bytes the host refused goes on without them, whatever the outcome.
var imageRules = [imageOutcomes]imageRule{
	imageDelivered:   {settle: true, window: closeWindow, counter: func(r *ImageResult) *int { return &r.Sent }},
	imageDeferred:    {window: keepWindow, counter: func(r *ImageResult) *int { return &r.Deferred }},
	imageStalled:     {attempt: true, window: stallWindow},
	imageFailed:      {attempt: true, window: failWindow, counter: func(r *ImageResult) *int { return &r.Failed }},
	imageUnavailable: {window: failWindow, counter: func(r *ImageResult) *int { return &r.Failed }},
}

// record does what imageRules says sent calls for with item, project q's; false when the
// project's other images wait for the window it opened.
func (p *imagePass) record(ctx context.Context, q *imageQueue, item imageItem, sent imageSend) bool {
	rule := imageRules[sent.outcome]
	now := time.Now()
	switch rule.window {
	case closeWindow:
		p.s.DestinationDelivered(q.id, now)
		p.accepted[q.sender.Endpoint] = true
	case stallWindow:
		f := Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: sent.err, RetryAt: p.s.DestinationStalled(q.id, now)}
		if !p.reported(q.id) {
			p.res.Stalls = append(p.res.Stalls, f)
		}
	case failWindow:
		f := Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: sent.err}
		// A pass cut short by its own deadline learned nothing about the host.
		if ctx.Err() == nil {
			f.RetryAt = p.s.DestinationFailedUntil(q.id, now, sent.retryAfter)
		}
		if !p.reported(q.id) {
			p.res.Failures = append(p.res.Failures, f)
		}
		p.errs = append(p.errs, fmt.Errorf("%s (%s): %w", q.id, q.sender.Endpoint, sent.err))
	}
	again := item.queued.Event
	if sent.refused {
		// The host would not take the bytes: the record goes on without them.
		again.Attrs = maps.Clone(again.Attrs)
		again.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedRefused
	}
	counter := rule.counter
	var ingest *IngestError
	switch {
	case rule.settle:
		p.settle(item.queued)
		p.ids = append(p.ids, imageID(item.out))
	case rule.attempt && sent.bare && again.Attempts >= imageMaxAttempts && errors.As(sent.err, &ingest) && ingest.Status/100 == 4:
		// Refused once more after imageMaxAttempts, carrying no bytes: the record itself.
		p.settle(item.queued)
		p.ids = append(p.ids, imageID(item.out))
		counter = func(r *ImageResult) *int { return &r.Rejected }
	case rule.attempt:
		again.Attempts++
		p.settle(item.queued, again)
	case sent.refused:
		p.settle(item.queued, again)
	}
	if counter != nil {
		*counter(&p.res)++
	}
	return rule.window == keepWindow || rule.window == closeWindow
}

// ImagesDue reports whether a pass of the image lane s has work at now: none runs, and a
// queued record's project is neither waiting out its images window nor held, or the
// record names none. It reads the queue without its lock, and only the records' projects.
func ImagesDue(s *spool.Spool, now time.Time) bool {
	if s.Busy() {
		return false
	}
	projects, err := s.Values(hookrun.AttrProjectID)
	if err != nil {
		return true
	}
	windows, holds := s.RetryWindows(now), s.Holds(now)
	for id := range projects {
		_, waiting := windows[id]
		if _, held := holds[id]; !waiting && !held {
			return true
		}
	}
	return false
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
func (r Router) forgetImages(ids []string, passStart time.Time, spools ...*spool.Spool) {
	promptimage.Prune(r.StateDir, passStart.Add(-spool.MaxAge))
	if len(ids) == 0 {
		return
	}
	named := map[string]bool{}
	for _, s := range spools {
		if s == nil {
			return
		}
		found, err := s.Mentions(ids)
		if err != nil {
			return
		}
		maps.Copy(named, found)
	}
	promptimage.Sweep(r.StateDir, ids, func(id string) bool { return named[id] }, passStart)
}
