package live

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Receiver is the OTLP/HTTP endpoint the harness exports to. It accepts what
// Terma's gateway accepts (protobuf or JSON over HTTP) and keeps every record
// as flat string attributes, which is all a contract needs.
type Receiver struct {
	srv  *http.Server
	addr string
	mu   sync.Mutex
	logs []LogRecord
	span []Span
	metr []string
	auth []string
}

// LogRecord is one OTLP log record, flattened.
type LogRecord struct {
	Time     time.Time
	Scope    string
	Body     string
	Attrs    map[string]string
	Resource map[string]string
}

// Span is one OTLP span, flattened.
type Span struct {
	Scope    string
	Name     string
	Attrs    map[string]string
	Resource map[string]string
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
	r.mu.Lock()
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.mu.Unlock()
	if strings.Contains(req.Header.Get("Content-Type"), "json") {
		return protojson.Unmarshal(body, msg)
	}
	return proto.Unmarshal(body, msg)
}

func (r *Receiver) handleLogs(w http.ResponseWriter, req *http.Request) {
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
				r.logs = append(r.logs, LogRecord{Time: at, Scope: sl.GetScope().GetName(), Body: anyString(lr.GetBody()),
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
				r.span = append(r.span, Span{Scope: ss.GetScope().GetName(), Name: sp.GetName(), Attrs: flatten(sp.GetAttributes()), Resource: res})
			}
		}
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	_, _ = w.Write(out)
}

func (r *Receiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	var in colmetricspb.ExportMetricsServiceRequest
	if err := r.read(req, &in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	r.mu.Lock()
	for _, rm := range in.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				r.metr = append(r.metr, m.GetName())
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
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
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
