package relay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// The vendor's copy of an agent's own telemetry (shape.Mirror): the relay takes it on the
// agent's own paths, sends the vendor an exact copy with the agent's own credential, and routes
// it as any export. The copy is sent for every session, claimed or not: pointing the agent at
// the relay must not change what its vendor receives. It goes out after the agent is answered,
// a few at a time; a copy the vendor refuses, or one with no room to wait, is counted and
// dropped, as the agent's own exporter would drop it.

// mirrorInFlight bounds the copies waiting on the vendor, and so the memory they hold.
const mirrorInFlight = 8

// mirrorTimeout bounds one copy's delivery.
const mirrorTimeout = 30 * time.Second

// vendorAuth is the context key of the Authorization an export arrived with, before the relay's
// own token took its place (Handler): the agent's credential for its vendor, which only the
// vendor's copy carries.
type vendorAuth struct{}

// mirrored serves one of m's paths for signal s.
func (r *Relay) mirrored(m shape.Mirror, s Signal) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Only the relay's own agents: never an open proxy to the vendor.
		if !r.authorized(req) {
			r.stats.add("refused_unauthorized", 1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(nil, req.Body, maxBody))
		if err != nil {
			r.stats.add("refused_unreadable", 1)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mirror(m, req, raw)
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.ContentLength = int64(len(raw))
		r.export(w, req, s)
	}
}

// mirror sends the vendor an exact copy of the export req carried, raw as it arrived.
func (r *Relay) mirror(m shape.Mirror, req *http.Request, raw []byte) {
	if m.Upstream == "" {
		return
	}
	header := http.Header{}
	for _, k := range []string{"Content-Type", "Content-Encoding", "User-Agent", "Accept"} {
		if v := req.Header.Get(k); v != "" {
			header.Set(k, v)
		}
	}
	if auth, _ := req.Context().Value(vendorAuth{}).(string); auth != "" {
		header.Set("Authorization", auth)
	}
	url := strings.TrimSuffix(m.Upstream, "/") + req.URL.Path
	select {
	case r.mirrorSlots <- struct{}{}:
	default:
		r.stats.add("mirrored.dropped_busy", 1)
		return
	}
	go func() {
		defer func() { <-r.mirrorSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), mirrorTimeout)
		defer cancel()
		out, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			r.stats.add("mirrored.failed", 1)
			return
		}
		out.Header = header
		resp, err := r.opts.HTTP.Do(out)
		if err != nil {
			r.stats.add("mirrored.failed", 1)
			r.debugf("relay: the vendor's copy to %s failed: %v", url, err)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			r.stats.add("mirrored.refused", 1)
			r.debugf("relay: the vendor refused its copy at %s: %s", url, resp.Status)
			return
		}
		r.stats.add("mirrored.sent", 1)
	}()
}

// debugf tells the debug log (TERMA_RELAY_DEBUG=1) what became of a copy: the vendor's copy
// failing is the vendor's and the agent's to notice, not an operator's warning.
func (r *Relay) debugf(format string, args ...any) {
	if r.opts.Logf != nil {
		r.opts.Logf(format, args...)
	}
}
