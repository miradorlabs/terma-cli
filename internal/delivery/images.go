package delivery

import (
	"cmp"
	"context"
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

// appendImageIDs adds each image record's image id to ids.
func appendImageIDs(ids []string, events ...spool.Event) []string {
	for _, e := range events {
		if id, ok := e.Attrs[semconv.TermaImageIDKey].(string); ok && isImage(e) {
			ids = append(ids, id)
		}
	}
	return ids
}

// FlushImages delivers the image lane, r.Images: each image in a request of its own, with
// its bytes, a project's images in the order of how often they stalled, so one that keeps
// stalling holds up none behind it. Its pass runs up to ImagePassTimeout, and only one runs
// at a time: another already delivering skips this one. events is the events' spool, which
// may still hold image records an older build spooled there; their bytes stay too.
//
// An image that stalls (the host was reached but the upload did not get through in its
// time) goes back behind its project's other images and opens the project's images window:
// the rest of its images wait for the window rather than spend the pass on an uplink that
// just failed one. After imageMaxStalls stalls its record is sent without its bytes.
func (r Router) FlushImages(ctx context.Context, events *spool.Spool, cfg *config.Config, force bool) Result {
	res := Result{Images: true}
	s := r.Images
	if s == nil {
		res.Err = errNoImageLane
		return res
	}
	now := time.Now()
	accepted := map[string]bool{}
	failed := 0
	// The images this pass sent or let go; their bytes stay until no waiting record names them.
	var ids []string
	router := spool.SenderFunc(func(ctx context.Context, records []spool.Event) ([]spool.Event, error) {
		byProject, held, gone := r.route(ctx, cfg, records, &res)
		ids = appendImageIDs(ids, gone...)
		// later is what waits for a later pass; stalled goes behind it.
		var later, stalled []spool.Event
		var errs []error
		for _, id := range slices.Sorted(maps.Keys(byProject)) {
			batch := byProject[id]
			sender, err := r.senderFor(cfg, id)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
				later = append(later, batch...)
				failed += len(batch)
				continue
			}
			if sender == nil {
				held = append(held, batch...)
				continue
			}
			if next := s.RetryAt(id); !force && now.Before(next) {
				res.Waiting = append(res.Waiting, Wait{ProjectID: id, RetryAt: next})
				later = append(later, batch...)
				res.Deferred += len(batch)
				continue
			}
			sender.Whole = true
			slices.SortStableFunc(batch, func(a, b spool.Event) int { return cmp.Compare(a.Stalls, b.Stalls) })
			for i, e := range batch {
				outcome, err := r.sendImage(ctx, sender, e)
				switch outcome {
				case imageDelivered:
					s.DestinationDelivered(id, time.Now())
					accepted[sender.Endpoint] = true
					res.Sent++
					ids = appendImageIDs(ids, e)
					continue
				case imageDeferred:
					// A smaller image may still fit in what is left of the pass.
					later = append(later, e)
					res.Deferred++
					continue
				case imageStalled:
					e.Stalls++
					stalled = append(stalled, e)
					res.Stalls = append(res.Stalls, Failure{ProjectID: id, Endpoint: sender.Endpoint, Err: err,
						RetryAt: s.DestinationStalled(id, time.Now())})
				case imageFailed:
					f := Failure{ProjectID: id, Endpoint: sender.Endpoint, Err: err}
					// A pass cut short by its own deadline learned nothing about the host.
					if ctx.Err() == nil {
						f.RetryAt = s.DestinationFailed(id, time.Now())
					}
					res.Failures = append(res.Failures, f)
					errs = append(errs, fmt.Errorf("%s (%s): %w", id, sender.Endpoint, err))
					later = append(later, e)
					failed++
				}
				// The project's other images wait for the window its stall or failure opened.
				rest := batch[i+1:]
				later = append(later, rest...)
				res.Deferred += len(rest)
				break
			}
		}
		if undelivered := slices.Concat(later, stalled); len(undelivered) > 0 {
			return held, &spool.PartialDelivery{Undelivered: undelivered, Err: errors.Join(errs...)}
		}
		return held, nil
	})
	// One read of the whole queue: a project's images are ordered among themselves, not
	// rotated batch by batch behind one that stalled.
	fr := s.Flush(ctx, router, spool.FlushOptions{Timeout: cmp.Or(r.imagePass, ImagePassTimeout), Batch: math.MaxInt,
		Force: force, NoWait: true, Now: now})
	if !fr.Skipped {
		r.forgetImages(ids, now, events, s)
	}
	res.Endpoints = slices.Sorted(maps.Keys(accepted))
	res.Held = fr.Held
	res.Failed = failed
	res.Expired = fr.Expired
	res.Pruned = fr.Pruned
	res.Dropped = fr.Dropped
	res.Skipped = fr.Skipped
	res.Reason = fr.Reason
	res.NextAttempt = s.NextAttempt()
	res.Err = fr.Err
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
	// imageStalled is an upload that reached the host and did not get through in its time:
	// news about the uplink, which backs off only the project's images.
	imageStalled
	// imageFailed is news about the host: its own answer, or a failure to reach it (DNS, the
	// dial, the TLS handshake, its timeout too).
	imageFailed
)

// judgeImage is how a send that ended in err went, in pass, having reached the host or not.
func judgeImage(pass context.Context, err error, reached bool) imageOutcome {
	var ingest *IngestError
	switch {
	case err == nil:
		return imageDelivered
	case errors.Is(err, errNoTimeLeft), errors.Is(pass.Err(), context.Canceled):
		return imageDeferred
	case errors.As(err, &ingest):
		return imageFailed
	case !reached && errors.Is(pass.Err(), context.DeadlineExceeded):
		return imageDeferred
	case !reached:
		return imageFailed
	case uploadBroke(err):
		return imageStalled
	}
	return imageFailed
}

// uploadBroke reports whether err is a request that ran out of time (its own or the pass's
// deadline) or lost its connection on the way.
func uploadBroke(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		slices.ContainsFunc(connectionBroken, func(errno syscall.Errno) bool { return errors.Is(err, errno) })
}

// sendImage sends one image record with its bytes from the store, and judges how it went.
// An image the platform refuses outright (a 400 or 413: the record, not the host or the
// key) goes again without them, saying so, rather than holding up every image after it for
// as long as it waits.
func (r Router) sendImage(ctx context.Context, sender *otlpSender, e spool.Event) (imageOutcome, error) {
	rec := withImageData(r.StateDir, e)
	reached, err := sender.post(ctx, wire([]spool.Event{rec}))
	var ingest *IngestError
	if _, carried := rec.Attrs[semconv.TermaImageDataKey]; carried && errors.As(err, &ingest) &&
		(ingest.Status == http.StatusBadRequest || ingest.Status == http.StatusRequestEntityTooLarge) {
		delete(rec.Attrs, semconv.TermaImageDataKey)
		rec.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedRefused
		reached, err = sender.post(ctx, wire([]spool.Event{rec}))
	}
	return judgeImage(ctx, err, reached), err
}

// forgetImages removes the stored bytes of the images ids this pass sent or let go that no
// record still queued in spools names: a replayed batch, or another prompt with the same
// image still waiting, keeps them. It reads spools in order, so the events' spool goes
// before the image lane's: the events' pass hands an image on to the lane before it lets
// it go from its own, and a record moving meanwhile is found in one or the other. It then
// prunes what outlived the spool's records.
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
	id, _ := e.Attrs[semconv.TermaImageIDKey].(string)
	if e.Stalls >= imageMaxStalls {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnsent
	} else if data, ok := promptimage.Load(stateDir, id); ok {
		e.Attrs[semconv.TermaImageDataKey] = data
	} else {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnavailable
	}
	return e
}
