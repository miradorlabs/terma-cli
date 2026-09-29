package live

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Receiver is the OTLP/HTTP endpoint the harness exports to. It accepts what
// Terma's gateway accepts (protobuf or JSON over HTTP) and keeps every record
// with their complete OTLP payloads as well as convenient flattened attributes.
type Receiver struct {
	srv      *http.Server
	addr     string
	mu       sync.Mutex
	logs     []LogRecord
	span     []Span
	metr     []Metric
	auth     []string
	requests []ExportRequest
	// refusal, when non-zero, is the status every export is answered with and not
	// recorded under: Terma's ingest down, for the relay's durability scenarios.
	refusal atomic.Int32
	// refused counts the exports answered with refusal.
	refused atomic.Int64
}

// Refuse answers every export with status (0: accept again) without recording it.
func (r *Receiver) Refuse(status int) { r.refusal.Store(int32(status)) }

// Refused is how many exports were answered with a refusal.
func (r *Receiver) Refused() int64 { return r.refused.Load() }

// refusing answers req with the refusal when one is set.
func (r *Receiver) refusing(w http.ResponseWriter) bool {
	code := r.refusal.Load()
	if code == 0 {
		return false
	}
	r.refused.Add(1)
	w.WriteHeader(int(code))
	return true
}

// LogRecord is one OTLP log record, flattened.
type LogRecord struct {
	Proto    *logspb.LogRecord
	Time     time.Time
	Scope    string
	Body     string
	Attrs    map[string]string
	Resource map[string]string
}

// Span is one OTLP span, flattened.
type Span struct {
	Proto    *tracepb.Span
	Scope    string
	Name     string
	Attrs    map[string]string
	Resource map[string]string
}

// Metric retains values, point attributes, temporality, buckets and exemplars.
type Metric struct {
	Proto    *metricspb.Metric
	Scope    string
	Resource map[string]string
}

// ExportRequest ties authorization to each signal, including rejected requests.
type ExportRequest struct {
	Path          string
	Authorization string
	Payload       proto.Message
	DecodeError   error
}

// StartReceiver listens on a loopback port for the test's lifetime.
func StartReceiver(t *testing.T) *Receiver {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &Receiver{addr: ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/logs", r.handleLogs)
	mux.HandleFunc("/v1/traces", r.handleTraces)
	mux.HandleFunc("/v1/metrics", r.handleMetrics)
	r.srv = &http.Server{Handler: mux}
	go func() { _ = r.srv.Serve(ln) }()
	t.Cleanup(func() { _ = r.srv.Close() })
	return r
}

// URL is the OTLP base endpoint.
func (r *Receiver) URL() string { return "http://" + r.addr }

func (r *Receiver) read(req *http.Request, msg proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(req.Body, 32<<20))
	if err != nil {
		return err
	}
	if strings.Contains(req.Header.Get("Content-Type"), "json") {
		// Decoded as Terma's gateway decodes it (gateways/otel/.../otlp_json.go): OTLP/JSON
		// writes trace and span ids as hex, which protojson alone would read as base64.
		if body, err = hexIDsToBase64(body); err == nil {
			err = protojson.Unmarshal(body, msg)
		}
	} else {
		err = proto.Unmarshal(body, msg)
	}
	r.mu.Lock()
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.requests = append(r.requests, ExportRequest{Path: req.URL.Path, Authorization: req.Header.Get("Authorization"), Payload: msg, DecodeError: err})
	r.mu.Unlock()
	return err
}

func (r *Receiver) handleLogs(w http.ResponseWriter, req *http.Request) {
	if r.refusing(w) {
		return
	}
	var in collogspb.ExportLogsServiceRequest
	if err := r.read(req, &in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	r.mu.Lock()
	for _, rl := range in.ResourceLogs {
		res := flatten(rl.GetResource().GetAttributes())
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				var at time.Time
				if lr.GetTimeUnixNano() != 0 {
					at = time.Unix(0, int64(lr.GetTimeUnixNano()))
				}
				r.logs = append(r.logs, LogRecord{Proto: lr, Time: at, Scope: sl.GetScope().GetName(), Body: anyString(lr.GetBody()),
					Attrs: flatten(lr.GetAttributes()), Resource: res})
			}
		}
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
	_, _ = w.Write(out)
}

func (r *Receiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	if r.refusing(w) {
		return
	}
	var in coltracepb.ExportTraceServiceRequest
	if err := r.read(req, &in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	r.mu.Lock()
	for _, rs := range in.ResourceSpans {
		res := flatten(rs.GetResource().GetAttributes())
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				r.span = append(r.span, Span{Proto: sp, Scope: ss.GetScope().GetName(), Name: sp.GetName(), Attrs: flatten(sp.GetAttributes()), Resource: res})
			}
		}
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	_, _ = w.Write(out)
}

func (r *Receiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	if r.refusing(w) {
		return
	}
	var in colmetricspb.ExportMetricsServiceRequest
	if err := r.read(req, &in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	r.mu.Lock()
	for _, rm := range in.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				r.metr = append(r.metr, Metric{Proto: m, Scope: sm.GetScope().GetName(), Resource: flatten(rm.GetResource().GetAttributes())})
			}
		}
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&colmetricspb.ExportMetricsServiceResponse{})
	_, _ = w.Write(out)
}

// Logs returns a copy of every log record so far.
func (r *Receiver) Logs() []LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LogRecord(nil), r.logs...)
}

// Spans returns a copy of every span so far.
func (r *Receiver) Spans() []Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Span(nil), r.span...)
}

// MetricNames returns the distinct metric names seen.
func (r *Receiver) MetricNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, n := range r.metr {
		name := n.Proto.GetName()
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Metrics returns the received metrics. Payloads must be treated as read-only.
func (r *Receiver) Metrics() []Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Metric(nil), r.metr...)
}

// Requests returns one entry per received export, rather than just the first key.
func (r *Receiver) Requests() []ExportRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ExportRequest(nil), r.requests...)
}

// Authorizations returns the Authorization headers seen, for the key contract.
func (r *Receiver) Authorizations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auth...)
}

// WaitLogs polls until at least one record satisfies pred, or the timeout.
func (r *Receiver) WaitLogs(timeout time.Duration, pred func(LogRecord) bool) []LogRecord {
	deadline := time.Now().Add(timeout)
	for {
		var hits []LogRecord
		for _, l := range r.Logs() {
			if pred(l) {
				hits = append(hits, l)
			}
		}
		if len(hits) > 0 || time.Now().After(deadline) {
			return hits
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func flatten(kvs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[kv.GetKey()] = anyString(kv.GetValue())
	}
	return out
}

func anyString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// otlpIDFields are the OTLP/JSON fields the specification writes as hex.
var otlpIDFields = map[string]bool{"traceId": true, "spanId": true, "parentSpanId": true}

// hexIDsToBase64 rewrites OTLP/JSON's hex trace and span ids as the base64 protojson
// reads, so a 16-byte trace id decodes to itself and not to 24 bytes of noise. Numbers
// are kept as they were written.
func hexIDsToBase64(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if s, ok := child.(string); ok && otlpIDFields[k] {
					if raw, err := hex.DecodeString(s); err == nil && (len(raw) == 16 || len(raw) == 8) {
						t[k] = base64.StdEncoding.EncodeToString(raw)
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
	return json.Marshal(doc)
}

// evidenceFor is what reached the receiver under one Authorization header: the part of
// the export a relay delivered with one project's key.
func (r *Receiver) evidenceFor(auth string) telemetryEvidence {
	var e telemetryEvidence
	for _, req := range r.Requests() {
		if req.Authorization != auth {
			continue
		}
		e.requests = append(e.requests, req)
		switch m := req.Payload.(type) {
		case *collogspb.ExportLogsServiceRequest:
			for _, rl := range m.ResourceLogs {
				res := flatten(rl.GetResource().GetAttributes())
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						var at time.Time
						if lr.GetTimeUnixNano() != 0 {
							at = time.Unix(0, int64(lr.GetTimeUnixNano()))
						}
						e.logs = append(e.logs, LogRecord{Proto: lr, Time: at, Scope: sl.GetScope().GetName(), Body: anyString(lr.GetBody()),
							Attrs: flatten(lr.GetAttributes()), Resource: res})
					}
				}
			}
		case *coltracepb.ExportTraceServiceRequest:
			for _, rs := range m.ResourceSpans {
				res := flatten(rs.GetResource().GetAttributes())
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						e.spans = append(e.spans, Span{Proto: sp, Scope: ss.GetScope().GetName(), Name: sp.GetName(), Attrs: flatten(sp.GetAttributes()), Resource: res})
					}
				}
			}
		case *colmetricspb.ExportMetricsServiceRequest:
			for _, rm := range m.ResourceMetrics {
				for _, sm := range rm.ScopeMetrics {
					for _, mt := range sm.Metrics {
						e.metrics = append(e.metrics, Metric{Proto: mt, Scope: sm.GetScope().GetName(), Resource: flatten(rm.GetResource().GetAttributes())})
					}
				}
			}
		}
	}
	return e
}
