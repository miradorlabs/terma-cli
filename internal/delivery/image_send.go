package delivery

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"syscall"
	"time"

	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Sending one image record, and judging how it went: the pass over the image queue
// (images.go) decides what that means for the record and its project.

// imageOutcome is how one image's send went.
type imageOutcome int

const (
	// imageDelivered is an image the host took.
	imageDelivered imageOutcome = iota
	// imageDeferred is an image cut off by its sender stopping (the relay, a manual flush's
	// interrupt): nothing learned.
	imageDeferred
	// imageStalled is an upload that reached the host and did not finish in its time: the
	// uplink. It counts an attempt.
	imageStalled
	// imageFailed is an image the host refused, or did not answer once its body was all
	// written (the last socket buffer of it may still have been on its way). It counts an
	// attempt.
	imageFailed
	// imageUnavailable is a host that could not take any record then: nothing about this
	// one, so it counts no attempt and keeps its place.
	imageUnavailable
	// imageOutcomes is how many outcomes there are.
	imageOutcomes
)

// judgeImage is how a send that ended in err went, in pass, having got as far as got.
//
// The host's answers:
//
//	2xx               delivered
//	400, 413          the bytes refused: sent again without them (sendImage)
//	401, 403          the key refused: unavailable
//	429, 502, 503     busy, or between deploys: unavailable, its Retry-After honoured
//	any other         failed: an attempt
//
// Short of an answer: cut off by its sender stopping is deferred; not reached (DNS, the
// dial, the TLS handshake) is unavailable; a body cut off mid-upload, in its own time,
// stalled; one all written and unanswered failed. What each outcome then does is
// imageRules'.
func judgeImage(sending context.Context, err error, got progress) imageOutcome {
	var ingest *IngestError
	switch {
	case err == nil:
		return imageDelivered
	case errors.As(err, &ingest) && hostUnavailable(ingest.Status):
		return imageUnavailable
	case errors.As(err, &ingest):
		return imageFailed
	case sending.Err() != nil:
		return imageDeferred
	case got == notReached:
		return imageUnavailable
	case got == reached && uploadBroke(err):
		return imageStalled
	}
	return imageFailed
}

// hostUnavailable reports whether status says nothing of the record: the key refused, or
// a host too busy or between deploys to take any.
func hostUnavailable(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	}
	return false
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

// imageSend is how one image's send went, with what its record needs next.
type imageSend struct {
	outcome imageOutcome
	err     error
	// refused says the host refused the image's bytes (400 or 413), and bare that the last
	// request carried none.
	refused, bare bool
	// retryAfter is when the host asked to be asked again; zero when it did not say.
	retryAfter time.Time
}

// sendImage sends one image record with its bytes from the store, in the time its size
// gives it (timeoutFor). One whose bytes the host refuses goes again without them, saying so.
func (r Router) sendImage(ctx context.Context, sender *otlpSender, e spool.Event) imageSend {
	if err := ctx.Err(); err != nil {
		return imageSend{outcome: judgeImage(ctx, err, notReached), err: err}
	}
	rec := withImageData(r.StateDir, e)
	_, carried := rec.Attrs[semconv.TermaImageDataKey]
	got, err := sender.post(ctx, wire([]spool.Event{rec}))
	var ingest *IngestError
	refused := carried && errors.As(err, &ingest) &&
		(ingest.Status == http.StatusBadRequest || ingest.Status == http.StatusRequestEntityTooLarge)
	if refused {
		delete(rec.Attrs, semconv.TermaImageDataKey)
		rec.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedRefused
		got, err = sender.post(ctx, wire([]spool.Event{rec}))
	}
	sent := imageSend{outcome: judgeImage(ctx, err, got), err: err, refused: refused, bare: !carried || refused}
	if errors.As(err, &ingest) {
		sent.retryAfter = ingest.RetryAfter
	}
	return sent
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
