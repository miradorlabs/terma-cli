package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/miradorlabs/terma-cli/internal/sendas"
)

const reasonOrgNotAccessible = "ORG_NOT_ACCESSIBLE"

func (s *sender) send(ctx context.Context, pol Policy, first entry, body []byte) (outcome, time.Duration, string, int64) {
	return s.r.post(ctx, pol, s.route.account, s.route.project, "", first.signal, body, nil)
}

func (r *Relay) post(ctx context.Context, pol Policy, account, project, enrollment string, sig Signal, body []byte, fence func() (outcome, time.Duration, string)) (outcome, time.Duration, string, int64) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(pol.Endpoint, "/")+"/v1/"+string(sig), bytes.NewReader(body))
	if err != nil {
		return refused, 0, "bad_endpoint", 0
	}
	bearer := pol.Key
	if bearer == "" {
		// The sign-in of the account that collected the parts (mayLeave), never whoever is
		// signed in now: Token gives none for another.
		token, err := r.opts.SignIn.Token(ctx, account)
		if err != nil {
			return keyless, keylessRetry, signInErrPrefix + err.Error(), 0
		}
		if token == "" {
			return keyless, keylessRetry, "no sign-in that may send these parts now", 0
		}
		bearer = token
		req.Header.Set(ProjectHeader, project)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("User-Agent", "terma-relay/"+r.opts.Version)
	if fence != nil {
		if out, wait, detail := fence(); out != sent {
			return out, wait, detail, 0
		}
	}
	resp, err := r.opts.HTTP.Do(req)
	if err != nil {
		return retry, 0, err.Error(), 0
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch code := resp.StatusCode; {
	case code/100 == 2:
		return sent, 0, "", partialSuccess(answer)
	case pol.Key == "" && sendas.Reason(answer) == sendas.ReasonSignInRetired:
		// Retired, whatever the status it comes with, as delivery and the heartbeat take it:
		// never refreshed as a token expired.
		r.opts.SignIn.Retired(bearer)
		return retry, refusedBackoff, fmt.Sprintf("HTTP %d %s: the sign-in is retired", code, sendas.ReasonSignInRetired), 0
	case code == http.StatusUnauthorized && pol.Key == "":
		// An expired or rotated token: the next attempt sends a fresh one.
		r.opts.SignIn.Refused(ctx, bearer)
		return retry, minBackoff, "HTTP 401: the sign-in's token was refused", 0
	case code == http.StatusForbidden && pol.Key == "":
		// Never call SignIn.Refused/Retired; nudge policy refresh on ORG_NOT_ACCESSIBLE.
		reason := sendas.Reason(answer)
		if reason == reasonOrgNotAccessible && enrollment != "" {
			r.nudgeRefresh(enrollment)
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

func (r *Relay) nudgeRefresh(enrollment string) {
	if r.opts.NudgeRefresh == nil {
		return
	}
	r.mu.Lock()
	if r.nudging[enrollment] {
		r.mu.Unlock()
		return
	}
	r.nudging[enrollment] = true
	r.mu.Unlock()
	go func() {
		defer func() { r.mu.Lock(); delete(r.nudging, enrollment); r.mu.Unlock() }()
		r.opts.NudgeRefresh(enrollment)
	}()
}

// signInErrPrefix starts the detail of a send whose sign-in's token could not be had, which
// is warned of: a refresh or keychain that keeps failing must still show.
const signInErrPrefix = "the sign-in: "

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
	teams, _ := r.outbox.teamRoutes()
	for _, rt := range teams {
		r.deliverMu.Lock()
		if entries, err := r.outbox.listTeam(rt); err == nil {
			entries = slices.DeleteFunc(entries, func(e entry) bool { return e.corrupt })
			if len(entries) > 0 {
				s := r.teamSender(rt)
				for _, e := range s.recover(entries) {
					r.stats.add("recovered_from_outbox", e.records)
				}
				s.poke()
			}
		}
		r.deliverMu.Unlock()
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
