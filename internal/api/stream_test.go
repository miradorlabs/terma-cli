package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sseServer(t *testing.T, body string, capture *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
}

// TestStream_ParsesFrames covers the SSE shapes the gateway actually emits:
// multi-line data, comment keep-alives, and events with an id to resume from.
func TestStream_ParsesFrames(t *testing.T) {
	body := ":ping\n" +
		"event: ready\ndata: {\"stream\":\"logs\"}\n\n" +
		"event: log\nid: abc123\ndata: {\"log\":\n" +
		"data: {\"body\":\"hello\"}}\n\n" +
		"event: heartbeat\ndata: {}\n\n"

	server := sseServer(t, body, nil)
	defer server.Close()

	client := newTestClient(t, server.URL, liveCredential(), "proj-1")
	stream, err := client.Stream(context.Background(), "/v1/logs/stream", nil, "")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	first, err := stream.Next()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if first.Name != "ready" {
		t.Errorf("first frame = %q, want ready (the comment must not become a frame)", first.Name)
	}

	second, err := stream.Next()
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if second.Name != "log" || second.ID != "abc123" {
		t.Errorf("second frame = %q id=%q, want log/abc123", second.Name, second.ID)
	}
	// Multi-line data joins with newlines per the SSE spec; dropping the join would
	// corrupt any record whose JSON the server split.
	if want := "{\"log\":\n{\"body\":\"hello\"}}"; second.Data != want {
		t.Errorf("data = %q, want %q", second.Data, want)
	}

	third, err := stream.Next()
	if err != nil {
		t.Fatalf("third frame: %v", err)
	}
	if third.Name != "heartbeat" {
		t.Errorf("third frame = %q, want heartbeat", third.Name)
	}

	if _, err := stream.Next(); err != io.EOF {
		t.Errorf("end of stream = %v, want io.EOF", err)
	}
}

// TestStream_SendsLastEventID is the resume contract: without this header the server
// replays the whole window instead of continuing after the last record seen.
func TestStream_SendsLastEventID(t *testing.T) {
	var got http.Header
	server := sseServer(t, "event: heartbeat\ndata: {}\n\n", &got)
	defer server.Close()

	client := newTestClient(t, server.URL, liveCredential(), "proj-1")
	stream, err := client.Stream(context.Background(), "/v1/logs/stream", nil, "cursor-42")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if v := got.Get("Last-Event-ID"); v != "cursor-42" {
		t.Errorf("Last-Event-ID = %q, want cursor-42", v)
	}
	if v := got.Get("Authorization"); !strings.HasPrefix(v, "Bearer ") {
		t.Errorf("stream request lost its credential: Authorization = %q", v)
	}
	// The project header decides which project the tail reads; a stream that omits
	// it would silently tail the wrong scope or 400.
	if v := got.Get(projectHeader); v != "proj-1" {
		t.Errorf("%s = %q, want proj-1", projectHeader, v)
	}
}

// TestStream_ErrorStatusIsReportedNotStreamed stops a 403 HTML page from being parsed
// as frames and surfacing as an empty, silent tail.
func TestStream_ErrorStatusIsReportedNotStreamed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"PERMISSION_DENIED","message":"no access to project"}}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, liveCredential(), "proj-1")
	_, err := client.Stream(context.Background(), "/v1/logs/stream", nil, "")
	if err == nil {
		t.Fatal("expected an error for a 403")
	}
	if !strings.Contains(err.Error(), "no access to project") {
		t.Errorf("error %q does not carry the gateway's message", err)
	}
}
