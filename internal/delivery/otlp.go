package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// otlpSender delivers each event as one OTLP/HTTP JSON log record named after it, with
// the project's server key, so the ingest host needs nothing terma-specific.
type otlpSender struct {
	// Endpoint is the OTLP base URL; /v1/logs is appended.
	Endpoint  string
	APIKey    string
	ProjectID string
	Version   string
	// UserID, set, names the Mirador user on the resource as terma.user.id, as the relay does.
	UserID string
	HTTP   *http.Client
	// timeout stands in for timeoutFor when set.
	timeout func(n int) time.Duration
}

const (
	otlpLogsPath = "/v1/logs"
	serviceName  = "terma-cli"
	sendTimeout  = 15 * time.Second
	// uploadFloor is the slowest uplink a request is given time for, past sendTimeout: a
	// request of an image's 2.8 MB has about a minute, one of small events 15 s.
	uploadFloor  = 64 << 10 // bytes a second
	severityInfo = 9
	severityText = "INFO"
)

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

// otlpValue is an OTLP AnyValue; an empty one is a null.
type otlpValue struct {
	String *string     `json:"stringValue,omitempty"`
	Int    *string     `json:"intValue,omitempty"`
	Double *float64    `json:"doubleValue,omitempty"`
	Bool   *bool       `json:"boolValue,omitempty"`
	Array  *otlpValues `json:"arrayValue,omitempty"`
	KVList *otlpKVList `json:"kvlistValue,omitempty"`
}

type otlpValues struct {
	Values []otlpValue `json:"values"`
}

type otlpKVList struct {
	Values []otlpAttr `json:"values"`
}

func attrOf(key string, v any) otlpAttr {
	return otlpAttr{Key: key, Value: valueOf(v)}
}

// attrsOf encodes attrs in key order. A spooled line decodes a list as []any and a map
// as map[string]any; an event not yet spooled may hold []string or []map[string]any.
func attrsOf(attrs map[string]any) []otlpAttr {
	out := make([]otlpAttr, 0, len(attrs))
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		out = append(out, attrOf(k, attrs[k]))
	}
	return out
}

func valueOf(v any) otlpValue {
	var a otlpValue
	switch x := v.(type) {
	case nil:
	case string:
		a.String = &x
	case bool:
		a.Bool = &x
	case int:
		s := strconv.Itoa(x)
		a.Int = &s
	case int64:
		s := strconv.FormatInt(x, 10)
		a.Int = &s
	case float64:
		if x == float64(int64(x)) {
			s := strconv.FormatInt(int64(x), 10)
			a.Int = &s
		} else {
			a.Double = &x
		}
	case []any:
		a.Array = arrayOf(x)
	case []string:
		a.Array = arrayOf(x)
	case []map[string]any:
		a.Array = arrayOf(x)
	case map[string]any:
		a.KVList = &otlpKVList{Values: attrsOf(x)}
	default:
		s := fmt.Sprint(v)
		a.String = &s
	}
	return a
}

func arrayOf[T any](items []T) *otlpValues {
	values := make([]otlpValue, 0, len(items))
	for _, item := range items {
		values = append(values, valueOf(item))
	}
	return &otlpValues{Values: values}
}

// timeoutFor is how long a request of n bytes may take.
func timeoutFor(n int) time.Duration {
	return sendTimeout + time.Duration(n)*time.Second/uploadFloor
}

// newHTTPClient refuses redirects: Go keeps the Authorization header on a same-host
// https→http downgrade, which would send the server key in cleartext.
// Each request's own deadline, which grows with its body, bounds it.
func newHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing to follow redirect to %s: the OTLP endpoint does not redirect log exports", req.URL.Redacted())
		},
	}
}

// progress is how far a request got before it ended.
type progress int32

const (
	// notReached is a request that never had a connection to the host: DNS, the dial or the
	// TLS handshake (its timeout too) failed, or nothing was tried.
	notReached progress = iota
	// reached is a request that had a connection and was writing its body.
	reached
	// written is a request whose body was all written, waiting for the host's answer. The
	// operating system may still be sending the body's last socket buffer.
	written
)

// progressTrace records in got how far a request gets. A request retried on a new
// connection, after a reused one failed, starts over.
func progressTrace(got *atomic.Int32) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectStart: func(string, string) { got.Store(int32(notReached)) },
		GotConn:      func(httptrace.GotConnInfo) { got.Store(int32(reached)) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				got.Store(int32(written))
			}
		},
	}
}

// Send implements spool.Sender.
func (o *otlpSender) Send(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
	_, err := o.post(ctx, events)
	return nil, err
}

// post sends events in one request, and reports how far it got.
func (o *otlpSender) post(ctx context.Context, events []spool.Event) (progress, error) {
	if o.Endpoint == "" || o.APIKey == "" {
		return notReached, fmt.Errorf("spool flush needs an OTLP endpoint and a team key — run `terma setup`")
	}
	records := make([]map[string]any, 0, len(events))
	for _, e := range events {
		attrs := attrsOf(e.Attrs)
		if e.SessionID != "" {
			attrs = append([]otlpAttr{attrOf(semconv.SessionIDKey, e.SessionID)}, attrs...)
		}
		ns := strconv.FormatInt(e.Time.UnixNano(), 10)
		record := map[string]any{
			"timeUnixNano":         ns,
			"observedTimeUnixNano": ns,
			"severityNumber":       severityInfo,
			"severityText":         severityText,
			"eventName":            e.Name,
			"body":                 map[string]string{"stringValue": e.Name},
			"attributes":           attrs,
		}
		if spool.ValidTraceID(e.TraceID) {
			record["traceId"] = e.TraceID
		}
		records = append(records, record)
	}
	resource := []otlpAttr{
		attrOf(semconv.ServiceNameKey, serviceName),
		attrOf(semconv.ServiceVersionKey, o.Version),
		attrOf(semconv.MiradorProjectIDKey, o.ProjectID),
		attrOf(semconv.TermaSchemaVersionKey, semconv.SchemaVersion),
	}
	if o.UserID != "" {
		resource = append(resource, attrOf(semconv.TermaUserIDKey, o.UserID))
	}
	payload := map[string]any{
		"resourceLogs": []map[string]any{{
			"resource": map[string]any{"attributes": resource},
			"scopeLogs": []map[string]any{{
				"scope":      map[string]string{"name": serviceName, "version": o.Version},
				"logRecords": records,
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return notReached, err
	}
	client := o.HTTP
	if client == nil {
		client = newHTTPClient()
	}
	timeout := timeoutFor(len(body))
	if o.timeout != nil {
		timeout = o.timeout(len(body))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var got atomic.Int32
	ctx = httptrace.WithClientTrace(ctx, progressTrace(&got))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Endpoint+otlpLogsPath, bytes.NewReader(body))
	if err != nil {
		return notReached, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("User-Agent", serviceName+"/"+o.Version)
	resp, err := client.Do(req)
	if err != nil {
		return progress(got.Load()), err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return written, &IngestError{Status: resp.StatusCode, Detail: string(bytes.TrimSpace(detail))}
	}
	return written, nil
}

// IngestError is a non-2xx answer from the ingest host.
type IngestError struct {
	Status int
	Detail string
}

func (e *IngestError) Error() string {
	return fmt.Sprintf("otlp ingest returned %d: %s", e.Status, e.Detail)
}

// KeyRefused reports whether the host rejected the credential rather than the request.
func (e *IngestError) KeyRefused() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}
