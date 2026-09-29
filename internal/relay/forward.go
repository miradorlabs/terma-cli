package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// Upstream. Each destination (a project's host and key) has its own queue and
// goroutine: parts leave in the order they were handed over, a transient failure is
// retried with backoff, and one refusing host never holds up another project.

// destQueue bounds one destination's queue, in parts; past it a part is dropped as
// upstream_queue_full.
const destQueue = 512

type destination struct {
	r      *Relay
	pol    Policy
	queue  chan *part
	mu     sync.Mutex
	active bool
}

// destination returns pol's destination, starting its sender on first use.
func (r *Relay) destination(pol Policy) *destination {
	key := pol.Endpoint + "\x00" + pol.Key
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.dests[key]
	if d == nil {
		d = &destination{r: r, pol: pol, queue: make(chan *part, destQueue)}
		r.dests[key] = d
		r.wg.Add(1)
		go d.drain()
	}
	return d
}

func (d *destination) enqueue(p *part) {
	select {
	case d.queue <- p:
	default:
		d.r.stats.dropped(p.signal, "upstream_queue_full", p.records)
	}
}

// busy reports whether the destination has anything queued or in flight.
func (d *destination) busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active || len(d.queue) > 0
}

// drain sends the queue in order until the relay stops, then empties it: each part
// still gets its send, bounded by the relay's grace, and what the grace cuts off is
// counted as lost.
func (d *destination) drain() {
	defer d.r.wg.Done()
	ctx := d.r.sendCtx
	sendOne := func(p *part) {
		d.mu.Lock()
		d.active = true
		d.mu.Unlock()
		d.send(ctx, p)
		d.mu.Lock()
		d.active = false
		d.mu.Unlock()
	}
	for {
		select {
		case p := <-d.queue:
			sendOne(p)
		case <-d.r.stopping:
			for {
				select {
				case p := <-d.queue:
					if ctx.Err() != nil {
						d.r.stats.dropped(p.signal, "upstream_lost_at_exit", p.records)
						continue
					}
					sendOne(p)
				default:
					return
				}
			}
		}
	}
}

// send delivers one part, retrying a transient failure (network, 429, 5xx) with
// backoff from one second to a minute. A refusal (any other 4xx) is final: the host
// will not take it however often it is asked.
func (d *destination) send(ctx context.Context, p *part) {
	body, err := proto.Marshal(p.msg)
	if err != nil {
		d.r.stats.dropped(p.signal, "encode_failed", p.records)
		return
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			d.r.stats.dropped(p.signal, "upstream_lost_at_exit", p.records)
			return
		}
		status, err := d.post(ctx, p.signal, body)
		switch {
		case err == nil && status/100 == 2:
			d.r.stats.forwarded(p.signal, p.records)
			return
		case err == nil && status != http.StatusTooManyRequests && status/100 == 4:
			d.r.stats.dropped(p.signal, fmt.Sprintf("upstream_refused_%d", status), p.records)
			return
		}
		d.r.stats.add("upstream_retries", 1)
		select {
		case <-ctx.Done():
			d.r.stats.dropped(p.signal, "upstream_lost_at_exit", p.records)
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (d *destination) post(ctx context.Context, s Signal, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.pol.Endpoint, "/")+"/v1/"+string(s), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+d.pol.Key)
	req.Header.Set("User-Agent", "terma-relay/"+d.r.opts.Version)
	resp, err := d.r.opts.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
