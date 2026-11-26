package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

const (
	// TargetBytesPerSec is the measured safe upload ceiling from MIR-717: 2 MiB/s of OTLP
	// request bytes, one request in flight at a time per route.
	TargetBytesPerSec = 2 << 20
	// defaultRequestTimeout gives a slow uplink twice the gateway's 30 s body read, so the
	// gateway, not this client, decides when a request was too slow.
	defaultRequestTimeout = 60 * time.Second
	defaultInitialBackoff = time.Second
	defaultMaxBackoff     = 60 * time.Second
	defaultMaxRetryAfter  = 5 * time.Minute
	defaultMaxRetries     = 5
	maxResponseBytes      = 64 << 10
	otlpLogsPath          = "/v1/logs"
)

// PermanentError reports that the ingest gateway permanently rejected records (for example,
// HTTP 200 OTLP partial_success with rejected_log_records > 0, or a non-retryable 4xx).
type PermanentError struct {
	Status   int
	Rejected int64
	Reason   string
}

func (e *PermanentError) Error() string { return e.Reason }

// OrgAccessLostError reports HTTP 403 ORG_NOT_ACCESSIBLE for one organization's team,
// without retiring the shared user sign-in token.
type OrgAccessLostError struct {
	Detail string
}

func (e *OrgAccessLostError) Error() string {
	if e.Detail == "" {
		return "organization is no longer accessible (ORG_NOT_ACCESSIBLE)"
	}
	return e.Detail
}

// SignInRetiredError reports SIGN_IN_RETIRED from the ingest gateway.
type SignInRetiredError struct {
	Detail string
}

func (e *SignInRetiredError) Error() string {
	if e.Detail == "" {
		return "sign-in is retired (SIGN_IN_RETIRED); run `terma setup`"
	}
	return e.Detail
}

// RouteError reports an authentication, authorization, or endpoint configuration failure
// (such as HTTP 401 after refresh, HTTP 403, or HTTP 404). Unlike PermanentError, it stops
// the route while leaving the current session in_progress so fixing credentials or
// configuration lets the next run resume it.
type RouteError struct {
	Status int
	Reason string
}

func (e *RouteError) Error() string { return e.Reason }

// TransientError reports a retryable delivery failure that exhausted its retry budget;
// the session remains in_progress so the next run resumes it.
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// SendRequest describes one OTLP /v1/logs POST on a route.
type SendRequest struct {
	Endpoint      string
	TeamID        string
	Account       string
	ServerKey     string
	Body          []byte
	BeforeAttempt func(ctx context.Context) error
}

// Transport delivers historical OTLP log chunks directly to /v1/logs without touching the
// shared live relay outbox. One pacer instance per route enforces serial delivery at 2 MiB/s.
type Transport struct {
	Version        string
	HTTP           *http.Client
	SignIn         sendas.SignIn
	BytesPerSec    int64
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxRetryAfter  time.Duration
	Now            func() time.Time
	Sleep          func(context.Context, time.Duration) error
}

// NewHTTPClient returns an isolated HTTP client that refuses redirects so bearer tokens
// can never be replayed or downgraded.
func NewHTTPClient() *http.Client {
	var tr http.RoundTripper
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	}
	return &http.Client{
		Timeout:   defaultRequestTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing redirect to %s: OTLP ingest does not redirect", req.URL.Redacted())
		},
	}
}

// RoutePacer paces serial uploads on one destination route at BytesPerSec.
type RoutePacer struct {
	t           *Transport
	nextAllowed time.Time
}

// NewRoutePacer returns a per-route pacer bound to t.
func (t *Transport) NewRoutePacer() *RoutePacer {
	return &RoutePacer{t: t}
}

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Transport) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if t.Sleep != nil {
		return t.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *Transport) rate() int64 {
	if t.BytesPerSec > 0 {
		return t.BytesPerSec
	}
	return TargetBytesPerSec
}

func (t *Transport) paceDuration(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(int64(n) * int64(time.Second) / t.rate())
}

// Send transmits req serially under this route's 2 MiB/s pacer, retrying 429, 5xx, 408,
// and transient network errors with exponential backoff honoring Retry-After.
func (p *RoutePacer) Send(ctx context.Context, req SendRequest) error {
	t := p.t
	client := t.HTTP
	if client == nil {
		client = NewHTTPClient()
		defer client.CloseIdleConnections()
	}
	maxRetries := t.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	backoff := t.InitialBackoff
	if backoff <= 0 {
		backoff = defaultInitialBackoff
	}
	maxBackoff := t.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	maxRetryAfter := t.MaxRetryAfter
	if maxRetryAfter <= 0 {
		maxRetryAfter = defaultMaxRetryAfter
	}

	refreshedOnce := false
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if wait := p.nextAllowed.Sub(t.now()); wait > 0 {
			if err := t.sleep(ctx, wait); err != nil {
				return err
			}
		}
		if req.BeforeAttempt != nil {
			if err := req.BeforeAttempt(ctx); err != nil {
				return err
			}
		}

		bearer, nameProject, err := t.credential(ctx, req)
		if err != nil {
			return err
		}

		sendStart := t.now()
		status, respBody, retryAfterVal, doErr := t.postOnce(ctx, client, req, bearer, nameProject)
		// Advance pacer by the OTLP request bytes sent so sustained throughput stays <= 2 MiB/s.
		base := sendStart
		if p.nextAllowed.After(base) {
			base = p.nextAllowed
		}
		p.nextAllowed = base.Add(t.paceDuration(len(req.Body)))

		if doErr != nil {
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ctx.Err()
			}
			if attempt >= maxRetries {
				return &TransientError{Err: fmt.Errorf("post %s: %w", otlpLogsPath, doErr)}
			}
			if err := t.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		if status/100 == 2 {
			if len(respBody) > maxResponseBytes {
				if attempt >= maxRetries {
					return &TransientError{Err: fmt.Errorf("ingest HTTP %d response exceeded %d bytes", status, maxResponseBytes)}
				}
				if err := t.sleep(ctx, backoff); err != nil {
					return err
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			rejected, errMsg, parseErr := relay.ParsePartialSuccess(respBody)
			if parseErr != nil {
				if attempt >= maxRetries {
					return &TransientError{Err: fmt.Errorf("invalid OTLP response on HTTP %d: %w", status, parseErr)}
				}
				if err := t.sleep(ctx, backoff); err != nil {
					return err
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			if rejected > 0 {
				reason := fmt.Sprintf("gateway rejected %d record(s)", rejected)
				if errMsg != "" {
					reason += ": " + errMsg
				}
				return &PermanentError{Status: status, Rejected: rejected, Reason: reason}
			}
			return nil
		}

		reason := sendas.Reason(respBody)
		if nameProject && reason == sendas.ReasonSignInRetired {
			if t.SignIn != nil {
				t.SignIn.Retired(bearer)
			}
			return &SignInRetiredError{Detail: fmt.Sprintf("HTTP %d %s: sign-in retired", status, reason)}
		}
		if status == http.StatusUnauthorized && nameProject && !refreshedOnce && t.SignIn != nil {
			refreshedOnce = true
			t.SignIn.Refused(ctx, bearer)
			continue
		}
		if status == http.StatusForbidden && nameProject && reason == "ORG_NOT_ACCESSIBLE" {
			return &OrgAccessLostError{Detail: fmt.Sprintf("HTTP 403 %s: organization not accessible", reason)}
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			return &RouteError{
				Status: status,
				Reason: formatHTTPFailure(status, respBody),
			}
		}
		if status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500 {
			if attempt >= maxRetries {
				return &TransientError{Err: fmt.Errorf("ingest returned HTTP %d after %d attempts", status, attempt+1)}
			}
			wait := max(backoff, parseRetryAfter(retryAfterVal, t.now(), maxRetryAfter))
			if err := t.sleep(ctx, wait); err != nil {
				return err
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		return &PermanentError{
			Status: status,
			Reason: formatHTTPFailure(status, respBody),
		}
	}
}

func (t *Transport) credential(ctx context.Context, req SendRequest) (bearer string, nameProject bool, err error) {
	if req.ServerKey != "" {
		return req.ServerKey, false, nil
	}
	if t.SignIn == nil || req.Account == "" {
		return "", false, &TransientError{Err: errors.New("no sign-in available for this account")}
	}
	tok, err := t.SignIn.Token(ctx, req.Account)
	if err != nil {
		return "", false, &TransientError{Err: fmt.Errorf("read sign-in token: %w", err)}
	}
	if tok == "" {
		return "", false, &TransientError{Err: errors.New("no active sign-in token for this account")}
	}
	return tok, true, nil
}

func (t *Transport) postOnce(ctx context.Context, client *http.Client, req SendRequest, bearer string, nameProject bool) (int, []byte, string, error) {
	target := strings.TrimRight(req.Endpoint, "/") + otlpLogsPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(req.Body))
	if err != nil {
		return 0, nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Authorization", "Bearer "+bearer)
	httpReq.Header.Set("User-Agent", "terma-history/"+t.Version)
	if nameProject && req.TeamID != "" {
		httpReq.Header.Set(sendas.ProjectHeader, req.TeamID)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return 0, nil, "", fmt.Errorf("read response body: %w", readErr)
	}
	return resp.StatusCode, body, resp.Header.Get("Retry-After"), nil
}

// parseRetryAfter parses either delta-seconds or an HTTP-date Retry-After header value.
func parseRetryAfter(v string, now time.Time, capWait time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return min(time.Duration(secs)*time.Second, capWait)
	}
	if at, err := http.ParseTime(v); err == nil {
		d := at.Sub(now)
		if d <= 0 {
			return 0
		}
		return min(d, capWait)
	}
	return 0
}

func formatHTTPFailure(status int, body []byte) string {
	detail := strings.TrimSpace(string(body))
	if len(detail) > 200 {
		detail = detail[:200] + "..."
	}
	if detail == "" {
		return fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
	}
	return fmt.Sprintf("HTTP %d: %s", status, detail)
}
