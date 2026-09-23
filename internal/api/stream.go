package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Event is one Server-Sent Events frame. Data is the raw JSON payload; the caller
// decodes it according to Name.
type Event struct {
	ID   string
	Name string
	Data string
}

// Stream is an open SSE connection. Close it to release the socket.
type Stream struct {
	resp    *http.Response
	scanner *bufio.Scanner
}

// maxFrameBytes bounds one frame. A log record with large attributes can exceed
// bufio.Scanner's 64 KiB default, which would end the stream mid-flight with
// ErrTooLong rather than skipping one record.
const maxFrameBytes = 4 << 20

// Stream opens a long-lived SSE connection to the data plane.
//
// It bypasses the shared http.Client because that one carries a request timeout,
// which for a streaming response covers reading the body too — a 30-second timeout
// would sever a healthy tail. Cancellation is the caller's ctx instead.
func (c *Client) Stream(ctx context.Context, path string, query url.Values, lastEventID string) (*Stream, error) {
	if err := c.refreshIfNeeded(ctx); err != nil {
		return nil, err
	}

	// A zero Timeout is the point: http.Client's timeout spans reading the body, so
	// the shared 30-second client would sever a healthy tail. ctx governs instead.
	// Redirects are refused here for the same reason as the unary client.
	client := &http.Client{Transport: c.http.Transport, CheckRedirect: refuseRedirects}

	gen := c.currentGen()
	resp, err := c.openStream(ctx, client, path, query, lastEventID)
	if err != nil {
		return nil, err
	}

	// A 401 on a token we believed was live means it was revoked or rotated elsewhere;
	// one refresh-and-retry recovers a tail whose token expired mid-stream, exactly as
	// the unary path in do() does.
	if resp.StatusCode == http.StatusUnauthorized && c.canRefresh() {
		resp.Body.Close()
		if refreshErr := c.refreshIfCurrent(ctx, gen); refreshErr != nil {
			return nil, refreshErr
		}
		resp, err = c.openStream(ctx, client, path, query, lastEventID)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, parseError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	return &Stream{resp: resp, scanner: scanner}, nil
}

func (c *Client) openStream(ctx context.Context, client *http.Client, path string, query url.Values, lastEventID string) (*http.Response, error) {
	headers := http.Header{}
	if lastEventID != "" {
		headers.Set("Last-Event-ID", lastEventID)
	}
	req, err := c.newRequest(ctx, dataHost, http.MethodGet, path, query, headers, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", path, err)
	}
	return resp, nil
}

// Next returns the next frame, blocking until one arrives. It returns io.EOF when
// the server closes the stream.
func (s *Stream) Next() (*Event, error) {
	var (
		event Event
		data  []string
	)
	for s.scanner.Scan() {
		line := s.scanner.Text()

		// A blank line terminates a frame. Frames carrying only a comment (the
		// keep-alive some proxies inject) have no data and are skipped.
		if line == "" {
			if len(data) == 0 {
				event = Event{}
				continue
			}
			event.Data = strings.Join(data, "\n")
			return &event, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			// A field with no colon is legal SSE and carries an empty value.
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "id":
			event.ID = value
		case "event":
			event.Name = value
		case "data":
			data = append(data, value)
		}
	}

	if err := s.scanner.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, io.EOF
		}
		return nil, err
	}
	return nil, io.EOF
}

// Close ends the stream by closing the response it reads from.
func (s *Stream) Close() error { return s.resp.Body.Close() }
