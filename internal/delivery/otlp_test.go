package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A spooled event leaves as one log record: its name in EventName alone, its trace in
// TraceId, lists as arrayValue and maps as kvlistValue, under terma's schema version.
func TestOTLPSenderEncodesTheRecord(t *testing.T) {
	t.Parallel()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const trace = "ae2a5e6f8e0168ae5ec2efa2c8772b02"
	for _, e := range []spool.Event{
		{Name: "terma.commit", SessionID: "s1", TraceID: trace, Attrs: map[string]any{
			"ids":   []string{"s1", "s2"},
			"stats": []map[string]any{{"path": "a.go", "lines_added": 2}},
			"args":  map[string]any{"cmd": "ls", "flags": []any{"-l"}},
			"count": 3,
		}},
		{Name: "terma.session.end", TraceID: "not-a-trace"},
	} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	sender := &otlpSender{Endpoint: srv.URL, APIKey: "k", ProjectID: "p1", Version: "v1", HTTP: srv.Client()}
	if res := s.Flush(context.Background(), sender, spool.FlushOptions{Force: true, Now: time.Now()}); res.Err != nil {
		t.Fatal(res.Err)
	}

	var payload struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []otlpAttr `json:"attributes"`
			} `json:"resource"`
			ScopeLogs []struct {
				LogRecords []struct {
					EventName  string     `json:"eventName"`
					TraceID    string     `json:"traceId"`
					Attributes []otlpAttr `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	resource := map[string]string{}
	for _, a := range payload.ResourceLogs[0].Resource.Attributes {
		resource[a.Key] = *a.Value.String
	}
	if resource[semconv.TermaSchemaVersionKey] != semconv.SchemaVersion || resource[semconv.MiradorProjectIDKey] != "p1" || resource[semconv.ServiceVersionKey] != "v1" {
		t.Fatalf("resource = %v", resource)
	}
	records := payload.ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(records) != 2 || records[0].EventName != "terma.commit" || records[0].TraceID != trace || records[1].TraceID != "" {
		t.Fatalf("records = %+v", records)
	}
	attrs := map[string]otlpValue{}
	for _, a := range records[0].Attributes {
		attrs[a.Key] = a.Value
	}
	if _, ok := attrs["event.name"]; ok {
		t.Error("event.name is sent as an attribute")
	}
	if *attrs[semconv.SessionIDKey].String != "s1" || *attrs["count"].Int != "3" {
		t.Errorf("scalars = %+v", attrs)
	}
	strs := func(v otlpValue) []string {
		var out []string
		for _, item := range v.Array.Values {
			out = append(out, *item.String)
		}
		return out
	}
	if got := strs(attrs["ids"]); !reflect.DeepEqual(got, []string{"s1", "s2"}) {
		t.Errorf("ids = %v", got)
	}
	stat := attrs["stats"].Array.Values[0].KVList.Values
	if len(stat) != 2 || stat[0].Key != "lines_added" || *stat[0].Value.Int != "2" || stat[1].Key != "path" || *stat[1].Value.String != "a.go" {
		t.Errorf("stats = %+v", stat)
	}
	args := attrs["args"].KVList.Values
	if len(args) != 2 || args[0].Key != "cmd" || *args[0].Value.String != "ls" || !reflect.DeepEqual(strs(args[1].Value), []string{"-l"}) {
		t.Errorf("args = %+v", args)
	}
}

func TestFlushStalledHTTPAllowsAppendAndRetainsQueue(t *testing.T) {
	// Serial: the 500 ms delivery deadline must outlast the server being scheduled.
	s, _ := spool.Open(t.TempDir())
	if err := s.Append(spool.Event{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	result := make(chan spool.Result, 1)
	go func() {
		result <- s.Flush(context.Background(), &otlpSender{Endpoint: srv.URL, APIKey: "test-key"}, spool.FlushOptions{Timeout: 500 * time.Millisecond})
	}()
	select {
	case <-entered:
	case res := <-result:
		t.Fatalf("flush returned before reaching endpoint: %+v", res)
	case <-time.After(3 * time.Second):
		t.Fatal("sender never reached endpoint")
	}
	// This uses the real append lock while the network request is stalled.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.AppendContext(ctx, spool.Event{Name: "during-send"}); err != nil {
		t.Fatalf("network delivery blocked append: %v", err)
	}
	select {
	case res := <-result:
		if !errors.Is(res.Err, context.DeadlineExceeded) || res.Sent != 0 {
			t.Fatalf("flush result: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("flush ignored its deadline")
	}
	if s.NextAttempt().IsZero() {
		t.Fatal("failed delivery must back off")
	}
	var names []string
	res := s.Flush(context.Background(), spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		for _, e := range events {
			names = append(names, e.Name)
		}
		return nil, nil
	}), spool.FlushOptions{Force: true})
	if res.Err != nil || strings.Join(names, ",") != "first,during-send" {
		t.Fatalf("retry lost/reordered events: %+v names=%v", res, names)
	}
	if n, _, err := s.Pending(); err != nil || n != 0 {
		t.Fatalf("queue after retry: n=%d err=%v", n, err)
	}
}
