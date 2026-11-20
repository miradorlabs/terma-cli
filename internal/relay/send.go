package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// send delivers one request and classifies the answer: 401, 403 and 404 are configuration fixed
// on this machine and retried slowly; only a body the host judges bad is refused for good.
// Without a server key it sends with the sign-in the route's parts were collected under,
// naming the team; a sign-in it cannot have now is retried later, as a refusal is.
func (s *sender) send(ctx context.Context, pol Policy, first entry, body []byte) (outcome, time.Duration, string, int64) {
	sig := first.signal
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(pol.Endpoint, "/")+"/v1/"+string(sig), bytes.NewReader(body))
	if err != nil {
		return refused, 0, "bad_endpoint", 0
	}
	bearer := pol.Key
	if bearer == "" {
		if s.r.opts.SignIn == nil {
			return retry, refusedBackoff, "no sign-in to send with", 0
		}
		// The sign-in of the account that collected the parts, never whoever is signed in
		// now: Token gives none for another.
		token, err := s.r.opts.SignIn.Token(ctx, s.r.owner(s.route, first))
		if err != nil {
			return retry, keylessRetry, "the sign-in: " + err.Error(), 0
		}
		if token == "" {
			return retry, keylessRetry, "no sign-in that may send these parts now", 0
		}
		bearer = token
		req.Header.Set(ProjectHeader, s.route.project)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("User-Agent", "terma-relay/"+s.r.opts.Version)
	resp, err := s.r.opts.HTTP.Do(req)
	if err != nil {
		return retry, 0, err.Error(), 0
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch code := resp.StatusCode; {
	case code/100 == 2:
		return sent, 0, "", partialSuccess(answer)
	case code == http.StatusUnauthorized && pol.Key == "":
		// An expired or rotated token: the next attempt sends a fresh one.
		s.r.opts.SignIn.Refused(ctx, bearer)
		return retry, minBackoff, "HTTP 401: the sign-in's token was refused", 0
	case code == http.StatusForbidden && pol.Key == "":
		// Only ORG_NOT_ACCESSIBLE is a lost membership, and even then nothing is dropped:
		// a membership granted back is refused until it is published, and the outbox ages
		// out what never leaves.
		reason := statusReason(answer)
		if reason == reasonSignInRetired {
			s.r.opts.SignIn.Retired()
		}
		return retry, refusedBackoff, fmt.Sprintf("HTTP 403 %s: the sign-in may not send to this team", reason), 0
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return retry, refusedBackoff, fmt.Sprintf("HTTP %d: the team's key was refused", code), 0
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code == http.StatusNotFound || code >= 500:
		return retry, retryAfter(resp.Header.Get("Retry-After")), fmt.Sprintf("HTTP %d", code), 0
	default:
		return refused, 0, "refused_" + strconv.Itoa(code), 0
	}
}

// partialSuccess reads the rejected count (field 1 of field 1) by hand, since the collector
// packages that define the response pull gRPC into every hook.
func partialSuccess(body []byte) int64 {
	ps := protoField(body, 1, protowire.BytesType)
	if ps == nil {
		return 0
	}
	for b := ps; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0
		}
		b = b[n:]
		if num == 1 && typ == protowire.VarintType {
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return 0
			}
			return max(0, int64(v))
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return 0
		}
		b = b[m:]
	}
	return 0
}

func protoField(b []byte, num protowire.Number, typ protowire.Type) []byte {
	for len(b) > 0 {
		n, t, l := protowire.ConsumeTag(b)
		if l < 0 {
			return nil
		}
		b = b[l:]
		if n == num && t == typ {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil
			}
			return v
		}
		m := protowire.ConsumeFieldValue(n, t, b)
		if m < 0 {
			return nil
		}
		b = b[m:]
	}
	return nil
}

func retryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

func nextBackoff(prev, floor time.Duration) time.Duration {
	next := max(minBackoff, prev*2, floor)
	limit := maxBackoff
	if floor >= refusedBackoff {
		limit = maxRefusedBackoff
	}
	return min(next, max(limit, floor))
}

// jitter spreads a wait over ±20% so relays that lost the gateway together do not return together.
func jitter(d, floor time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) * 2 / 5
	return max(floor, d-time.Duration(spread/2)+time.Duration(rand.Int64N(spread+1)))
}

func (r *Relay) recoverOutbox() {
	routes, _ := r.outbox.routes()
	for _, rt := range routes {
		r.recoverRoute(rt)
	}
}

// recoverRoute queues what a previous relay left on rt. It holds deliverMu so an export
// arriving meanwhile is never between writing its part and queueing it: the part would be
// listed here, then pushed again, and sent twice.
func (r *Relay) recoverRoute(rt route) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	entries, err := r.outbox.list(rt)
	if err != nil || len(entries) == 0 {
		return
	}
	s := r.sender(rt)
	n := 0
	for _, e := range s.recover(entries) {
		n += e.records
	}
	r.stats.add("recovered_from_outbox", n)
	s.poke()
}
