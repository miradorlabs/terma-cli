package delivery

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"syscall"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A terma.prompt.image record leaves in a lane of its own. Hooks spool it to the image
// spool (spool.ImageDir), apart from the events, and a detached pass of its own sends it,
// so no event waits on an upload. The spool holds what the record says of the image, never
// the bytes, which wait in the image store (promptimage) and join the record only in its
// own request: no request outgrows what the platform takes in one. The store forgets the
// bytes once no record still queued names them.

// ImagePassTimeout bounds a pass of the image lane. The largest image's request
// (promptimage.MaxBytes in base64, about 2.8 MB) is given timeoutFor's minute on the slowest
// uplink it allows for, 64 KiB/s, and is started only with all of it left: the pass has room
// for five of those, where the events' 30 s (spool.DefaultFlushTimeout) has room for none.
const ImagePassTimeout = 5 * time.Minute

// imageMaxStalls is how many times an image's upload may stall before its record is sent
// without its bytes (unsent): each stall spent up to a minute of the uplink on it.
const imageMaxStalls = 4

// imageEnvelope bounds what an image record's request holds besides the image: the
// record's other attributes and the OTLP envelope around it.
const imageEnvelope = 8 << 10

// errNoTimeLeft is an image not sent because too little of the pass was left for it.
var errNoTimeLeft = errors.New("too little of the pass left for the image")

// ImageResult summarizes one pass of the image lane; its counters are disjoint, one per
// reason, and count image records.
type ImageResult struct {
	Sent, Held, Failed, Expired, Pruned, Dropped, Unroutable, Withheld int
	// Deferred counts the images left untried for a later pass: their project's images
	// wait out a window, too little of the pass was left for one, or the pass ended first.
	Deferred int
	Skipped  bool
	Reason   spool.SkipReason
	Err      error
	// Failures names each project whose image the host refused or could not be reached for.
	Failures []Failure
	// Stalls names each project whose image did not get through in its time, one record each:
	// news about the uplink, not the host, so the pass does not fail for it.
	Stalls []Failure
	// Waiting names each project whose images window was open.
	Waiting   []Wait
	Endpoints []string
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

// FlushImages delivers the image lane, r.Images: each image in a request of its own, with
// its bytes, settled in the queue as soon as it went, so a pass cut short resends none.
// Projects take turns, one image each, so no team's backlog fills the pass, and a project's
// images go in the order of how often they stalled. Its pass runs up to ImagePassTimeout,
// and only one runs at a time: another already delivering skips this one. events is the
// events' spool, which may still hold image records an older build spooled there; their
// bytes stay too.
//
// An image that stalls (its upload did not get through in its time) goes to the back of
// the queue and opens its project's images window: the project's other images wait for the
// window rather than spend the pass on an uplink that just failed one. After imageMaxStalls
// stalls its record is sent without its bytes.
func (r Router) FlushImages(ctx context.Context, events *spool.Spool, cfg *config.Config, force bool) ImageResult {
	var res ImageResult
	s := r.Images
	if s == nil {
		res.Err = errNoImageLane
		return res
	}
	now := time.Now()
	accepted := map[string]bool{}
	var errs []error
	// The images this pass sent or let go; their bytes stay until no waiting record names them.
	var ids []string
	settle := func(p spool.Pending, requeue ...spool.Event) {
		if err := s.Settle(p, requeue...); err != nil {
			errs = append(errs, fmt.Errorf("settle an image record: %w", err))
		}
	}
	fr := s.Drain(ctx, spool.DrainOptions{Timeout: cmp.Or(r.imagePass, ImagePassTimeout), NoWait: true, Now: now},
		func(ctx context.Context, queued []spool.Pending) {
			byProject := map[string][]imageItem{}
			places := r.places(cfg)
			for _, p := range queued {
				id, out, place := places.of(ctx, p.Event)
				switch place {
				case placeSend:
					byProject[id] = append(byProject[id], imageItem{queued: p, out: out})
					continue
				case placeHeld:
					res.Held++
					continue
				case placeUnroutable:
					res.Unroutable++
				case placeWithheld:
					res.Withheld++
				}
				settle(p)
				ids = append(ids, imageID(p.Event))
			}
			var turns []*imageQueue
			for _, id := range slices.Sorted(maps.Keys(byProject)) {
				items := byProject[id]
				// The window first: a project waiting out one needs no key this pass.
				if next := s.RetryAt(id); !force && now.Before(next) {
					res.Waiting = append(res.Waiting, Wait{ProjectID: id, RetryAt: next})
					res.Deferred += len(items)
					continue
				}
				sender, err := r.senderFor(cfg, id)
				if err != nil {
					// The keychain is locked or out of reach: the next pass asks again.
					errs = append(errs, fmt.Errorf("%s: %w", id, err))
					res.Failed += len(items)
					continue
				}
				if sender == nil {
					res.Held += len(items)
					continue
				}
				slices.SortStableFunc(items, func(a, b imageItem) int { return cmp.Compare(a.out.Stalls, b.out.Stalls) })
				turns = append(turns, &imageQueue{id: id, sender: sender, items: items})
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
					outcome, err := r.sendImage(ctx, q.sender, item.out)
					switch outcome {
					case imageDelivered:
						settle(item.queued)
						s.DestinationDelivered(q.id, time.Now())
						accepted[q.sender.Endpoint] = true
						res.Sent++
						ids = append(ids, imageID(item.out))
					case imageDeferred:
						res.Deferred++
					case imageStalled:
						item.out.Stalls++
						settle(item.queued, item.out)
						res.Stalls = append(res.Stalls, Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: err,
							RetryAt: s.DestinationStalled(q.id, time.Now())})
					case imageFailed:
						f := Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: err}
						// A pass cut short by its own deadline learned nothing about the host.
						if ctx.Err() == nil {
							f.RetryAt = s.DestinationFailed(q.id, time.Now())
						}
						res.Failures = append(res.Failures, f)
						errs = append(errs, fmt.Errorf("%s (%s): %w", q.id, q.sender.Endpoint, err))
						res.Failed++
					}
					if outcome == imageStalled || outcome == imageFailed {
						// The project's other images wait for the window its stall or failure opened.
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
		r.forgetImages(ids, now, events, s)
	}
	res.Endpoints = slices.Sorted(maps.Keys(accepted))
	res.Expired = fr.Expired
	res.Pruned = fr.Pruned
	res.Dropped = fr.Dropped
	res.Skipped = fr.Skipped
	res.Reason = fr.Reason
	res.Err = errors.Join(append([]error{fr.Err}, errs...)...)
	return res
}

// imageOutcome is how one image's send went, for what the lane does next.
type imageOutcome int

const (
	// imageDelivered is an image the host took.
	imageDelivered imageOutcome = iota
	// imageDeferred is an image left for a later pass having learned nothing: too little of
	// the pass was left to start it, the pass was cancelled, or it ran out before the host
	// was reached.
	imageDeferred
	// imageStalled is an upload that reached the host and did not finish writing its body in
	// its time: news about the uplink, which backs off only the project's images.
	imageStalled
	// imageFailed is news about the host: its own answer, no answer in time to a body all
	// written, or a failure to reach it (DNS, the dial, the TLS handshake, its timeout too).
	imageFailed
)

// judgeImage is how a send that ended in err went, in pass, having got as far as got. A
// body all written that the host did not answer in time is the host's, though the last
// socket buffer of it may still have been on its way: its window backs off all the same.
func judgeImage(pass context.Context, err error, got progress) imageOutcome {
	var ingest *IngestError
	switch {
	case err == nil:
		return imageDelivered
	case errors.Is(err, errNoTimeLeft), errors.Is(pass.Err(), context.Canceled):
		return imageDeferred
	case errors.As(err, &ingest):
		return imageFailed
	case got == notReached && errors.Is(pass.Err(), context.DeadlineExceeded):
		return imageDeferred
	case got == reached && uploadBroke(err):
		return imageStalled
	}
	return imageFailed
}

// uploadBroke reports whether err is a request that ran out of time (its own or the pass's
// deadline) or lost its connection on the way: the peer broke it off, or the transport
// closed it under the write once it saw that (net.ErrClosed).
func uploadBroke(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		slices.ContainsFunc(connectionBroken, func(errno syscall.Errno) bool { return errors.Is(err, errno) })
}

// sendImage sends one image record with its bytes from the store, and judges how it went.
// An image is started only with all its request's time left, judged from the size its
// record names before its bytes are read. An image the platform refuses outright (a 400 or
// 413: the record, not the host or the key) goes again without them, saying so, rather than
// holding up every image after it for as long as it waits.
func (r Router) sendImage(ctx context.Context, sender *otlpSender, e spool.Event) (imageOutcome, error) {
	if err := ctx.Err(); err != nil {
		return judgeImage(ctx, err, notReached), err
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < r.timeout(imageRequestSize(e)) {
		return imageDeferred, errNoTimeLeft
	}
	rec := withImageData(r.StateDir, e)
	got, err := sender.post(ctx, wire([]spool.Event{rec}))
	var ingest *IngestError
	if _, carried := rec.Attrs[semconv.TermaImageDataKey]; carried && errors.As(err, &ingest) &&
		(ingest.Status == http.StatusBadRequest || ingest.Status == http.StatusRequestEntityTooLarge) {
		delete(rec.Attrs, semconv.TermaImageDataKey)
		rec.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedRefused
		got, err = sender.post(ctx, wire([]spool.Event{rec}))
	}
	return judgeImage(ctx, err, got), err
}

// imageRequestSize is how large e's request will be at most, from the size its record names:
// its bytes in base64 when it will carry them, and its envelope.
func imageRequestSize(e spool.Event) int {
	if _, omitted := e.Attrs[semconv.TermaImageOmittedKey]; omitted || e.Stalls >= imageMaxStalls {
		return imageEnvelope
	}
	// A spooled number decodes as float64; a record not yet spooled holds an int.
	size := promptimage.MaxBytes
	switch n := e.Attrs[semconv.TermaImageSizeKey].(type) {
	case float64:
		size = int(n)
	case int:
		size = n
	}
	return base64.StdEncoding.EncodedLen(min(max(size, 0), promptimage.MaxBytes)) + imageEnvelope
}

// timeout is how long a request of n bytes may take.
func (r Router) timeout(n int) time.Duration {
	if r.requestTimeout != nil {
		return r.requestTimeout(n)
	}
	return timeoutFor(n)
}

// forgetImages removes the stored bytes of the images ids this pass sent or let go that no
// record still queued in spools names: a replayed batch, or another prompt with the same
// image still waiting, keeps them. It reads spools in order, so the events' spool goes
// before the image lane's: the events' pass hands an image on to the lane before it lets
// it go from its own, and a record moving meanwhile is found in one or the other. It then
// prunes what outlived the spool's records.
//
// A record parked in a repository's git directory (hookrun's park) is in no spool: when the
// same image's other record is sent first, the parked one later leaves saying its bytes are
// unavailable. Parking is a sandboxed hook's last resort and the same image pasted twice
// across it rare, so that is accepted rather than searched for in every repository.
func (r Router) forgetImages(ids []string, passStart time.Time, spools ...*spool.Spool) {
	if len(ids) > 0 {
		named := map[string]bool{}
		read := true
		for _, s := range spools {
			if s == nil {
				continue
			}
			found, err := s.Mentions(ids)
			if err != nil {
				read = false
				break
			}
			maps.Copy(named, found)
		}
		if read {
			promptimage.Sweep(r.StateDir, ids, func(id string) bool { return named[id] }, passStart)
		}
	}
	promptimage.Prune(r.StateDir, passStart.Add(-spool.MaxAge))
}

// withImageData is the image record e with its bytes from the store, or saying why it has
// none: they are gone, or its upload stalled too often to try again.
func withImageData(stateDir string, e spool.Event) spool.Event {
	if _, omitted := e.Attrs[semconv.TermaImageOmittedKey]; omitted {
		return e
	}
	e.Attrs = maps.Clone(e.Attrs)
	if e.Stalls >= imageMaxStalls {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnsent
	} else if data, ok := promptimage.Load(stateDir, imageID(e)); ok {
		e.Attrs[semconv.TermaImageDataKey] = data
	} else {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnavailable
	}
	return e
}
