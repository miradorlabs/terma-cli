package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// post delivers one request and classifies the answer: 401, 403 and 404 are configuration fixed
// on this machine and retried slowly; only a body the host judges bad is refused for good.
func (r *Relay) post(ctx context.Context, pol Policy, sig Signal, body []byte) (outcome, time.Duration, string, int64) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(pol.Endpoint, "/")+"/v1/"+string(sig), bytes.NewReader(body))
	if err != nil {
		return refused, 0, "bad_endpoint", 0
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+pol.Key)
	req.Header.Set("User-Agent", "terma-relay/"+r.opts.Version)
	resp, err := r.opts.HTTP.Do(req)
	if err != nil {
		return retry, 0, err.Error(), 0
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch code := resp.StatusCode; {
	case code/100 == 2:
		return sent, 0, "", partialSuccess(answer)
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
