package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Reading the web. A page that is not there (404, 410) is errNotFound, which a source takes for
// "this vendor has no such page"; anything else that fails is an error, which a source takes
// for "not read this time", so that a vendor's outage never reads as its models going away.

var errNotFound = errors.New("not found")

const userAgent = "terma-model-reference/1 (+https://terma.ai/models)"

// maxBody bounds what one response may hold: the largest page read is a vendor's changelog,
// well under it.
const maxBody = 16 << 20

type client struct {
	http *http.Client
	// tries is how many times a request that failed for a reason that may pass (a timeout, a
	// 429, a 5xx) is made in all.
	tries int
	wait  time.Duration
	// rewrite, in tests, sends a request for one URL to another (a local server's).
	rewrite func(url string) string
}

func newClient() *client {
	return &client{http: &http.Client{Timeout: 60 * time.Second}, tries: 3, wait: 2 * time.Second}
}

// text is the body of a GET of url.
func (c *client) text(ctx context.Context, url string) (string, error) {
	body, _, err := c.do(ctx, http.MethodGet, url, nil, nil)
	return string(body), err
}

// getJSON decodes the body of a GET of url, with headers, into v.
func (c *client) getJSON(ctx context.Context, url string, headers map[string]string, v any) error {
	body, _, err := c.do(ctx, http.MethodGet, url, headers, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %w", redactURL(url), err)
	}
	return nil
}

// apiResponse is an API's answer whatever its status: a probe reads a refusal as much as an
// acceptance.
type apiResponse struct {
	Status  int
	Header  http.Header
	Body    []byte
	Elapsed time.Duration
}

// post sends body as JSON and returns the API's answer, whatever its status. It is tried again
// only where the request never reached an answer the API meant (a 429, a 5xx, the network).
func (c *client) post(ctx context.Context, url string, headers map[string]string, body any) (*apiResponse, error) {
	if c.rewrite != nil {
		url = c.rewrite(url)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var last *apiResponse
	var lastErr error
	for try := range c.tries {
		if try > 0 {
			if err := sleep(ctx, c.backoff(try, last)); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		start := time.Now()
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		last = &apiResponse{Status: resp.StatusCode, Header: resp.Header, Body: b, Elapsed: time.Since(start)}
		lastErr = nil
		if !retryable(resp.StatusCode) {
			return last, nil
		}
	}
	if last != nil {
		return last, nil
	}
	return nil, lastErr
}

func (c *client) do(ctx context.Context, method, url string, headers map[string]string, body []byte) ([]byte, http.Header, error) {
	if c.rewrite != nil {
		url = c.rewrite(url)
	}
	var lastErr error
	var lastStatus int
	var lastHeader http.Header
	for try := range c.tries {
		if try > 0 {
			var r *apiResponse
			if lastStatus != 0 {
				r = &apiResponse{Status: lastStatus, Header: lastHeader}
			}
			if err := sleep(ctx, c.backoff(try, r)); err != nil {
				return nil, nil, err
			}
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			lastStatus = 0
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		switch {
		case err != nil:
			lastErr = err
			lastStatus = 0
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			return nil, resp.Header, fmt.Errorf("%s: %w", redactURL(url), errNotFound)
		case resp.StatusCode/100 == 2:
			return b, resp.Header, nil
		default:
			lastErr = fmt.Errorf("%s: %s", redactURL(url), resp.Status)
			lastStatus, lastHeader = resp.StatusCode, resp.Header
			if !retryable(resp.StatusCode) {
				return nil, resp.Header, lastErr
			}
		}
	}
	return nil, nil, lastErr
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status/100 == 5
}

// backoff is how long to wait before try: what the server's retry-after asks, within reason,
// or a wait that doubles.
func (c *client) backoff(try int, last *apiResponse) time.Duration {
	if last != nil {
		if s, err := strconv.Atoi(last.Header.Get("Retry-After")); err == nil && s > 0 && s <= 60 {
			return time.Duration(s) * time.Second
		}
	}
	return c.wait << (try - 1)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// redactURL is url without its query, where an API key may ride (Gemini's ?key=).
func redactURL(url string) string {
	if i := strings.IndexByte(url, '?'); i >= 0 {
		return url[:i]
	}
	return url
}
