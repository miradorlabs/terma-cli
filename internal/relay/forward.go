package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Destination is where one project's records are delivered.
type Destination struct {
	// Endpoint is the OTLP/HTTP base URL; /v1/<signal> is appended.
	Endpoint string
	// Authorization is the header value sent with every request (the project's key).
	Authorization string
}

// ErrHeld is returned by a DestinationFunc when a project cannot be delivered to yet —
// no key for it on this machine. Its records wait.
var ErrHeld = errors.New("held")

// DestinationFunc says where a project's records go. It is called before every delivery,
// so a key stored after the relay started takes effect without a restart.
type DestinationFunc func(project string) (Destination, error)

// MachineProjectFunc names the machine project, "" while none is chosen. It is called
// before every delivery of the machine project's records, so a project chosen by
// `terma setup` after the relay started takes effect without a restart.
type MachineProjectFunc func() string

const (
	sendTimeout   = 15 * time.Second
	maxMergeBytes = 4 << 20
	maxMergeFiles = 64
	minBackoff    = time.Second
	maxBackoff    = 2 * time.Minute
	// maxRetryAfter bounds how long a gateway's Retry-After can park a project.
	maxRetryAfter = 10 * time.Minute
	// A refused key is not fixed by retrying soon; it waits for `terma install`.
	refusedBackoff    = 5 * time.Minute
	maxRefusedBackoff = time.Hour
	heldRetry         = time.Minute
	idleWait          = 30 * time.Second
)

// forwarders runs one delivery loop per route, started the first time the route has
// something to send.
type forwarders struct {
	ctx     context.Context
	dir     string
	dest    DestinationFunc
	machine MachineProjectFunc
	client  *http.Client
	version string
	logf    func(string, ...any)
	stats   *stats

	mu   sync.Mutex
	m    map[string]chan struct{}
	wg   sync.WaitGroup
	hook func(route string, delivered int) // tests: told after each delivery
}

// wake makes route's loop look at its outbox now, starting the loop if needed.
func (f *forwarders) wake(route string) {
	if !validRoute(route) {
		return
	}
	f.mu.Lock()
	ch, ok := f.m[route]
	if !ok {
		ch = make(chan struct{}, 1)
		f.m[route] = ch
		f.wg.Go(func() {
			f.loop(route, ch)
		})
	}
	f.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (f *forwarders) wait() { f.wg.Wait() }

func (f *forwarders) loop(route string, wake <-chan struct{}) {
	dir := filepath.Join(f.dir, outboxDir, route)
	backoff := time.Duration(0)
	// single sends one body per request after a merged request was refused, so one bad
	// body is set aside without the ones merged with it.
	single := false
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-f.ctx.Done():
			return false
		case <-t.C:
			return true
		case <-wake:
			// New work does not cut a backoff short: the host that failed is the same.
			if backoff > 0 {
				select {
				case <-f.ctx.Done():
					return false
				case <-t.C:
				}
			}
			return true
		}
	}
	for f.ctx.Err() == nil {
		entries, err := listEntries(dir)
		if err != nil {
			f.logf("list %s: %v", route, err)
			if !sleep(idleWait) {
				return
			}
			continue
		}
		if len(entries) == 0 {
			f.stats.setHeld(route, false)
			if !sleep(idleWait) {
				return
			}
			continue
		}
		project, dest, err := f.destination(route)
		if err != nil {
			f.stats.setHeld(route, true)
			if !errors.Is(err, ErrHeld) {
				f.logf("destination for %s: %v", route, err)
			}
			if !sleep(heldRetry) {
				return
			}
			continue
		}
		f.stats.setHeld(route, false)
		if single {
			entries = entries[:1]
		}
		batch, body, err := f.take(dir, entries)
		if err != nil {
			f.logf("read %s: %v", route, err)
			if !sleep(minBackoff) {
				return
			}
			continue
		}
		outcome, wait, detail, rejected := f.send(dest, batch[0], body)
		switch outcome {
		case sent:
			if rejected > 0 {
				// Accepted, but the gateway dropped some of it: resending would only
				// duplicate what it kept, so it is counted and said, not retried.
				f.stats.addRejected(rejected)
				f.logf("%s accepted %s but rejected %d records: %s", project, batch[0].name, rejected, detail)
			}
			single = false
			f.remove(dir, batch)
			f.stats.addDelivered(len(batch))
			f.stats.clearError(project)
			backoff = 0
			if f.hook != nil {
				f.hook(route, len(batch))
			}
		case refused:
			if len(batch) > 1 {
				single = true
				continue
			}
			single = false
			f.stats.addDead(len(batch))
			f.stats.setError(project, detail)
			f.logf("%s refused %s: %s", project, batch[0].name, detail)
			f.bury(dir, route, batch)
		case retry:
			f.stats.setError(project, detail)
			backoff = nextBackoff(backoff, wait)
			if !sleep(jitter(backoff, wait)) {
				return
			}
		}
	}
}

// destination resolves a route to its project and where that project's records go.
func (f *forwarders) destination(route string) (string, Destination, error) {
	project := route
	if route == machineRoute {
		if project = f.machine(); project == "" {
			return "", Destination{}, ErrHeld
		}
	}
	dest, err := f.dest(project)
	return project, dest, err
}

// take reads the oldest entries of one signal that fit one request: JSON bodies are
// merged up to maxMergeBytes / maxMergeFiles; a protobuf body travels alone.
func (f *forwarders) take(dir string, entries []entry) ([]entry, []byte, error) {
	first := entries[0]
	body, err := os.ReadFile(filepath.Join(dir, first.name))
	if err != nil {
		return nil, nil, err
	}
	if first.format != formatJSON {
		return entries[:1], body, nil
	}
	batch := []entry{first}
	bodies := [][]byte{body}
	size := len(body)
	for _, e := range entries[1:] {
		if len(batch) >= maxMergeFiles || e.sig != first.sig || e.format != formatJSON {
			break
		}
		b, err := os.ReadFile(filepath.Join(dir, e.name))
		if err != nil || size+len(b) > maxMergeBytes {
			break
		}
		batch = append(batch, e)
		bodies = append(bodies, b)
		size += len(b)
	}
	merged, err := mergeBodies(first.sig, bodies)
	if err != nil {
		// One unreadable body must not hold the rest back: send the first alone and let
		// the backend judge it.
		return entries[:1], body, nil
	}
	return batch, merged, nil
}

type outcome int

const (
	sent outcome = iota
	retry
	refused
)

// send delivers one request and classifies the answer. A refused key (401, 403) and a
// missing endpoint (404) are retried, slowly: they are configuration, fixed on this
// machine, not a fault in the records. Only a body the backend judges malformed is
// refused for good. A 2xx can still carry OTLP's partial success: how many records the
// gateway rejected out of an accepted request, returned as rejected.
func (f *forwarders) send(dest Destination, e entry, body []byte) (outcome, time.Duration, string, int64) {
	ctx, cancel := context.WithTimeout(f.ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest.Endpoint+e.sig.path(), bytes.NewReader(body))
	if err != nil {
		return refused, 0, err.Error(), 0
	}
	if e.format == formatJSON {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Content-Type", "application/x-protobuf")
	}
	req.Header.Set("Authorization", dest.Authorization)
	req.Header.Set("User-Agent", "terma-relay/"+f.version)
	resp, err := f.client.Do(req)
	if err != nil {
		return retry, 0, err.Error(), 0
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	snippet := answer[:min(len(answer), 512)]
	detail := fmt.Sprintf("HTTP %d %s", resp.StatusCode, bytes.TrimSpace(snippet))
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		if e.format != formatJSON {
			return sent, 0, "", 0
		}
		rejected, message := partialSuccess(answer)
		return sent, 0, message, rejected
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return retry, refusedBackoff, detail + " — the project's key was refused; `terma install` in the repository stores a new one", 0
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code == http.StatusNotFound || code >= 500:
		return retry, retryAfter(resp.Header.Get("Retry-After")), detail, 0
	default:
		return refused, 0, detail, 0
	}
}

// partialSuccess reads OTLP/JSON's partial success from an export response: the records
// the gateway rejected (one of the three counts, by signal) and its message. protojson
// writes int64 as a string, which json.Number takes as it takes a number.
func partialSuccess(body []byte) (int64, string) {
	var resp struct {
		PartialSuccess struct {
			Logs    json.Number `json:"rejectedLogRecords"`
			Spans   json.Number `json:"rejectedSpans"`
			Points  json.Number `json:"rejectedDataPoints"`
			Message string      `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return 0, ""
	}
	ps := resp.PartialSuccess
	return sumCounts(ps.Logs.String(), ps.Spans.String(), ps.Points.String()), ps.Message
}

func sumCounts(counts ...string) int64 {
	var total int64
	for _, c := range counts {
		if n, err := strconv.ParseInt(c, 10, 64); err == nil && n > 0 {
			total += n
		}
	}
	return total
}

func retryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

// nextBackoff doubles the previous wait within its bounds, starting from floor when the
// answer asked for one.
func nextBackoff(prev, floor time.Duration) time.Duration {
	next := max(minBackoff, prev*2, floor)
	limit := maxBackoff
	if floor >= refusedBackoff {
		limit = maxRefusedBackoff
	}
	return min(next, max(limit, floor))
}

// jitter spreads a wait over ±20%, never below floor (a gateway's Retry-After). Without
// it, every relay that lost the gateway in the same outage retries on the same beat and
// meets the recovering gateway all at once.
func jitter(d, floor time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) * 2 / 5
	return max(floor, d-time.Duration(spread/2)+time.Duration(rand.Int64N(spread+1)))
}

func (f *forwarders) remove(dir string, batch []entry) {
	for _, e := range batch {
		if err := os.Remove(filepath.Join(dir, e.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.logf("remove %s: %v", e.name, err)
		}
	}
}

// bury moves refused bodies to dead/, where the janitor bounds them.
func (f *forwarders) bury(dir, route string, batch []entry) {
	dead := filepath.Join(f.dir, deadDir)
	if err := os.MkdirAll(dead, 0o700); err != nil {
		f.logf("dead letter: %v", err)
		f.remove(dir, batch)
		return
	}
	for _, e := range batch {
		if err := os.Rename(filepath.Join(dir, e.name), filepath.Join(dead, route+"-"+e.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.logf("dead letter %s: %v", e.name, err)
		}
	}
}
