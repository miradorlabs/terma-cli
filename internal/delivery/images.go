package delivery

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A terma.prompt.image record leaves as no other does. The spool holds what it says of the
// image, never the bytes, which wait in the image store (promptimage) and join the record
// only in its own request: no batch outgrows what the platform takes in one, and an image
// holds up nothing sent before it. The store forgets the bytes once no record still queued
// names them.

// isImage reports whether e is a terma.prompt.image record.
func isImage(e spool.Event) bool { return e.Name == semconv.TermaPromptImageEvent }

// appendImage adds e's image id to ids, when e is an image record.
func appendImage(ids []string, e spool.Event) []string {
	if id, ok := e.Attrs[semconv.TermaImageIDKey].(string); ok && isImage(e) {
		return append(ids, id)
	}
	return ids
}

// imageRequests splits a project's batch into its requests: its other events in one, then
// an image to a request. A project whose images stalled earlier in the pass sends only the
// first; its images are later, for the next pass.
func imageRequests(batch []spool.Event, stalled bool) (requests [][]spool.Event, later []spool.Event) {
	var rest []spool.Event
	for _, e := range batch {
		if !isImage(e) {
			rest = append(rest, e)
		}
	}
	if len(rest) > 0 {
		requests = append(requests, rest)
	}
	for _, e := range batch {
		switch {
		case !isImage(e):
		case stalled:
			later = append(later, e)
		default:
			requests = append(requests, []spool.Event{e})
		}
	}
	return requests, later
}

// imageStalled reports whether an image's request failed for want of time or a connection
// rather than by the host's answer: a slow uplink, not news about the host, so it backs off
// neither the project nor its other events.
func imageStalled(ctx context.Context, req []spool.Event, err error) bool {
	var ingest *IngestError
	return len(req) == 1 && isImage(req[0]) && ctx.Err() == nil && !errors.As(err, &ingest)
}

// send sends one request; an image's carries its bytes from the store. An image the platform
// refuses outright (a 400 or 413: the record, not the host or the key) goes again without
// them, saying so, rather than holding up every image after it for as long as it waits.
func (r Router) send(ctx context.Context, sender *otlpSender, req []spool.Event) error {
	if len(req) != 1 || !isImage(req[0]) {
		_, err := sender.Send(ctx, wire(req))
		return err
	}
	e := withImageData(r.StateDir, req[0])
	_, err := sender.Send(ctx, wire([]spool.Event{e}))
	var ingest *IngestError
	if _, carried := e.Attrs[semconv.TermaImageDataKey]; !carried || !errors.As(err, &ingest) ||
		ingest.Status != http.StatusBadRequest && ingest.Status != http.StatusRequestEntityTooLarge {
		return err
	}
	delete(e.Attrs, semconv.TermaImageDataKey)
	e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedRefused
	_, err = sender.Send(ctx, wire([]spool.Event{e}))
	return err
}

// forgetImages removes the stored bytes of the images ids this pass sent or let go that no
// record still in the spool names: a replayed batch, or another prompt with the same image
// still waiting, keeps them. It then prunes what outlived the spool's records.
func (r Router) forgetImages(s *spool.Spool, ids []string, passStart time.Time) {
	if len(ids) > 0 {
		if named, err := s.Mentions(ids); err == nil {
			promptimage.Sweep(r.StateDir, ids, func(id string) bool { return named[id] }, passStart)
		}
	}
	promptimage.Prune(r.StateDir, passStart.Add(-spool.MaxAge))
}

// withImageData is the image record e with its bytes from the store, or saying they are gone.
func withImageData(stateDir string, e spool.Event) spool.Event {
	if _, omitted := e.Attrs[semconv.TermaImageOmittedKey]; omitted {
		return e
	}
	e.Attrs = maps.Clone(e.Attrs)
	id, _ := e.Attrs[semconv.TermaImageIDKey].(string)
	if data, ok := promptimage.Load(stateDir, id); ok {
		e.Attrs[semconv.TermaImageDataKey] = data
	} else {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnavailable
	}
	return e
}
