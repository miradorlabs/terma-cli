package delivery

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"slices"
	"syscall"
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
// first, then oldest. A send that reaches the host and does not get through counts an
// attempt, goes to the back of the queue and opens the project's images window. Only one
// pass runs at a time. events is the events' spool, whose records from an older build may
// still name stored bytes; nil keeps every stored image.
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
			for _, id := range startingAfter(slices.Sorted(maps.Keys(byProject)), s.Mark()) {
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
				slices.SortStableFunc(items, func(a, b imageItem) int {
					return cmp.Or(cmp.Compare(a.out.Attempts, b.out.Attempts), a.out.Time.Compare(b.out.Time))
				})
				turns = append(turns, &imageQueue{id: id, sender: sender, items: items})
			}
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
					outcome, err := r.sendImage(ctx, q.sender, item.out)
					switch outcome {
					case imageDelivered:
						settle(item.queued)
						s.DestinationDelivered(q.id, time.Now())
						accepted[q.sender.Endpoint] = true
						res.Sent++
						ids = append(ids, imageID(item.out))
						if len(q.items) > 0 {
							next = append(next, q)
						}
						continue
					case imageDeferred:
						res.Deferred++
						if len(q.items) > 0 {
							next = append(next, q)
						}
						continue
					case imageStalled:
						res.Stalls = append(res.Stalls, Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: err,
							RetryAt: s.DestinationStalled(q.id, time.Now())})
					case imageFailed, imageUnreached:
						f := Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: err}
						// A pass cut short by its own deadline learned nothing about the host.
						if ctx.Err() == nil {
							f.RetryAt = s.DestinationFailed(q.id, time.Now())
						}
						res.Failures = append(res.Failures, f)
						errs = append(errs, fmt.Errorf("%s (%s): %w", q.id, q.sender.Endpoint, err))
						res.Failed++
					}
					if outcome != imageUnreached {
						// An attempt the record may be to blame for: it goes to the back.
						again := item.queued.Event
						again.Attempts++
						settle(item.queued, again)
					}
					// The project's other images wait for the window just opened.
					res.Deferred += len(q.items)
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

// ImagesDue reports whether a pass of the image lane s has work at now: a queued record
// whose project's images window is closed, or that names no project. A queue it cannot
// read is due, for the pass to sort out.
func ImagesDue(s *spool.Spool, now time.Time) bool {
	queued, err := s.Peek(math.MaxInt)
	if err != nil {
		return true
	}
	windows := s.RetryWindows(now)
	for _, e := range queued {
		id, _ := e.Attrs[hookrun.AttrProjectID].(string)
		if _, waiting := windows[id]; !waiting {
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

// imageOutcome is how one image's send went.
type imageOutcome int

const (
	// imageDelivered is an image the host took.
	imageDelivered imageOutcome = iota
	// imageDeferred is an image untried, or cut off by the pass having learned nothing.
	imageDeferred
	// imageStalled is an upload that reached the host and did not finish in its time: the
	// uplink. It counts an attempt.
	imageStalled
	// imageFailed is an image the host refused, or did not answer once its body was all
	// written (the last socket buffer of it may still have been on its way). It counts an
	// attempt.
	imageFailed
	// imageUnreached is a host that could not be reached (DNS, the dial, the TLS handshake)
	// or refused the key: nothing about the record, so it counts no attempt.
	imageUnreached
)

// judgeImage is how a send that ended in err went, in pass, having got as far as got.
func judgeImage(pass context.Context, err error, got progress) imageOutcome {
	var ingest *IngestError
	switch {
	case err == nil:
		return imageDelivered
	case errors.Is(err, errNoTimeLeft), errors.Is(pass.Err(), context.Canceled):
		return imageDeferred
	case errors.As(err, &ingest) && ingest.KeyRefused():
		return imageUnreached
	case errors.As(err, &ingest):
		return imageFailed
	case got == notReached && errors.Is(pass.Err(), context.DeadlineExceeded):
		return imageDeferred
	case got == notReached:
		return imageUnreached
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
// It starts only with all its request's time left, judged from the record's size before the
// bytes are read. One refused outright (400 or 413) goes again without them, saying so.
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
	if _, omitted := e.Attrs[semconv.TermaImageOmittedKey]; omitted || e.Attempts >= imageMaxAttempts {
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

// withImageData is the image record e with its bytes from the store, or saying why it has
// none: they are gone, or it was tried too often.
func withImageData(stateDir string, e spool.Event) spool.Event {
	if _, omitted := e.Attrs[semconv.TermaImageOmittedKey]; omitted {
		return e
	}
	e.Attrs = maps.Clone(e.Attrs)
	if e.Attempts >= imageMaxAttempts {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnsent
	} else if data, ok := promptimage.Load(stateDir, imageID(e)); ok {
		e.Attrs[semconv.TermaImageDataKey] = data
	} else {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnavailable
	}
	return e
}
