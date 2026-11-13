package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// What a pass over the image queue does with each image once it is sent: the outcome table
// (imageRules) and record, which follows it.

// settle takes the event taken out of the queue, appending requeue at its tail.
func (p *imagePass) settle(taken *spool.Taken, requeue ...spool.Event) {
	if err := taken.Settle(requeue...); err != nil {
		p.errs = append(p.errs, fmt.Errorf("settle an image record: %w", err))
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
func (p *imagePass) record(ctx context.Context, q *imageQueue, item imageItem, taken *spool.Taken, sent imageSend) bool {
	rule := imageRules[sent.outcome]
	now := p.now()
	switch rule.window {
	case closeWindow:
		p.s.DestinationDelivered(q.id, now)
		p.accepted[q.sender.Endpoint] = true
	case stallWindow:
		f := Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: sent.err, RetryAt: p.s.DestinationStalled(q.id, now)}
		p.res.Stalls = append(p.res.Stalls, f)
	case failWindow:
		f := Failure{ProjectID: q.id, Endpoint: q.sender.Endpoint, Err: sent.err}
		// A sender stopping mid-send learned nothing about the host.
		if ctx.Err() == nil {
			until := time.Time{}
			if sent.retryAfter > 0 {
				until = now.Add(sent.retryAfter)
			}
			f.RetryAt = p.s.DestinationFailedUntil(q.id, now, until)
		}
		p.res.Failures = append(p.res.Failures, f)
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
		p.settle(taken)
		p.ids = append(p.ids, imageID(item.out))
	case rule.attempt && sent.bare && again.Attempts >= imageMaxAttempts && errors.As(sent.err, &ingest) && ingest.Status/100 == 4:
		// Refused once more after imageMaxAttempts, carrying no bytes: the record itself.
		p.settle(taken)
		p.ids = append(p.ids, imageID(item.out))
		counter = func(r *ImageResult) *int { return &r.Rejected }
	case rule.attempt:
		again.Attempts++
		p.settle(taken, again)
	case sent.refused:
		p.settle(taken, again)
	}
	if counter != nil {
		*counter(&p.res)++
	}
	return rule.window == keepWindow || rule.window == closeWindow
}
