package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Two probes a profile takes beside the provider's requests: the payloads a build hands its
// hooks ("hooks"), and how it exports its own telemetry ("telemetry"): each metric's type,
// unit and temporality, each span's parent, and the protocol and interval of each signal's
// export, measured from when the receiver got them.

// HookShape is one hook event's payloads, as the "hooks" probe records them.
type HookShape struct {
	Keys  []string `json:"keys"`
	Count int      `json:"count"`
}

// RecordHooks records, as probe "hooks", the payload of every hook b ran in sb: by the event
// name the payload gives (hook_event_name), else the terma hook it ran, each with the dotted key
// paths of its payloads, three deep. The recording shim in front of terma kept them.
func RecordHooks(t *testing.T, b Binary, sb *Sandbox) {
	t.Helper()
	events := HookShapes(sb.payloadDir())
	if len(events) == 0 {
		t.Logf("%s %s ran no hook", b.Harness, b.Version)
		return
	}
	RecordProbe(t, b, "hooks", map[string]any{"events": events})
}

// HookShapes are the payloads in dir by event.
func HookShapes(dir string) map[string]*HookShape {
	entries, _ := os.ReadDir(dir)
	out := map[string]*HookShape{}
	opts := StorageOptions{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		event, _ := payload["hook_event_name"].(string)
		if event == "" {
			_, event, _ = strings.Cut(strings.TrimSuffix(e.Name(), ".json"), "-")
		}
		s := out[event]
		if s == nil {
			s = &HookShape{}
			out[event] = s
		}
		s.Count++
		s.Keys = append(s.Keys, opts.keyPaths(payload, 3)...)
	}
	for _, s := range out {
		s.Keys = sortedUnique(s.Keys)
	}
	return out
}

// otlpArrival is one export the timing receiver passed on: when it came, how, and what it held.
type otlpArrival struct {
	Signal      string // logs | metrics | traces
	At          time.Time
	ContentType string
	Encoding    string
	Payload     proto.Message
}

// TimedReceiver stands in front of a sandbox's receiver: it notes when each export arrives and
// hands it on unchanged, so the receiver keeps everything it would have.
type TimedReceiver struct {
	URL      string
	mu       sync.Mutex
	arrivals []otlpArrival
}

// TimeReceiver starts a timing front for r, for t's lifetime.
func TimeReceiver(t *testing.T, r *Receiver) *TimedReceiver {
	t.Helper()
	tr := &TimedReceiver{}
	mux := http.NewServeMux()
	for signal, next := range map[string]http.HandlerFunc{"logs": r.handleLogs, "metrics": r.handleMetrics, "traces": r.handleTraces} {
		mux.HandleFunc("/v1/"+signal, func(w http.ResponseWriter, req *http.Request) {
			body, _ := io.ReadAll(io.LimitReader(req.Body, 32<<20))
			tr.note(signal, req.Header, body)
			req.Body = io.NopCloser(bytes.NewReader(body))
			next(w, req)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tr.URL = srv.URL
	return tr
}

func (tr *TimedReceiver) note(signal string, h http.Header, body []byte) {
	a := otlpArrival{Signal: signal, At: time.Now().UTC(), ContentType: h.Get("Content-Type"), Encoding: h.Get("Content-Encoding")}
	switch signal {
	case "logs":
		a.Payload = &collogspb.ExportLogsServiceRequest{}
	case "metrics":
		a.Payload = &colmetricspb.ExportMetricsServiceRequest{}
	default:
		a.Payload = &coltracepb.ExportTraceServiceRequest{}
	}
	var err error
	if strings.Contains(a.ContentType, "json") {
		err = protojson.Unmarshal(otlpJSONIDs(body), a.Payload)
	} else {
		err = proto.Unmarshal(body, a.Payload)
	}
	if err != nil {
		a.Payload = nil
	}
	tr.mu.Lock()
	tr.arrivals = append(tr.arrivals, a)
	tr.mu.Unlock()
}

func (tr *TimedReceiver) snapshot() []otlpArrival {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]otlpArrival(nil), tr.arrivals...)
}

// TelemetryShape is what the "telemetry" probe records.
type TelemetryShape struct {
	Surfaces map[string]*SurfaceShape `json:"surfaces"`
	Export   ExportShape              `json:"export"`
}

// SurfaceShape is a metric's kind or a span's place, by its surface as the field census names
// it (fields.go): "metrics/<name>", "traces/<span>".
type SurfaceShape struct {
	Type        string `json:"type,omitempty"` // sum | gauge | histogram | exponential_histogram | summary
	Unit        string `json:"unit,omitempty"`
	Temporality string `json:"temporality,omitempty"` // delta | cumulative
	Monotonic   *bool  `json:"monotonic,omitempty"`
	// Parent is the surface of a span's parent, Parents every one where it has several; Root
	// marks a span seen without one.
	Parent  string   `json:"parent,omitempty"`
	Parents []string `json:"parents,omitempty"`
	Root    bool     `json:"root,omitempty"`
}

// ExportShape is how the build exported: the protocol of each signal ("http/protobuf",
// "http/json"; Protocol where they agree), its compression, and the interval of each signal's
// periodic export.
type ExportShape struct {
	Protocol    string            `json:"protocol,omitempty"`
	Protocols   map[string]string `json:"protocols,omitempty"`
	Compression string            `json:"compression,omitempty"`
	// IntervalMS is each signal's export interval, measured: for logs and spans, how long the
	// oldest record of a batch waited for it, at most the shortest gap between two batches; for
	// metrics, the span of the first periodic collection, then the gap between collections. Only what arrived before the turn's last
	// model call was answered counts, so the exit's flush is not taken for a period. Each is
	// rounded to the nearest of a few round values, so a night's jitter is not a change.
	IntervalMS map[string]int64 `json:"interval_ms,omitempty"`
	// IntervalBatches are how many exports of each signal arrived before the cutoff: what its
	// interval rests on. One batch's wait carries any stall of the build's own (a busy event
	// loop), so an interval from one batch is a weaker measure than one from several.
	IntervalBatches map[string]int `json:"interval_batches,omitempty"`
}

// RecordTelemetry records, as probe "telemetry", what tr passed on of b's export; cutoff is
// when the turn's last model call was answered.
func RecordTelemetry(t *testing.T, b Binary, tr *TimedReceiver, cutoff time.Time) {
	t.Helper()
	shape := TelemetryShapeOf(tr.snapshot(), cutoff)
	if len(shape.Surfaces) == 0 && len(shape.Export.IntervalMS) == 0 && shape.Export.Protocol == "" && len(shape.Export.Protocols) == 0 {
		t.Logf("%s %s exported nothing to the timing receiver", b.Harness, b.Version)
		return
	}
	RecordProbe(t, b, "telemetry", shape)
}

// TelemetryShapeOf reads arrivals. terma's own records (TermaService) are not the build's.
func TelemetryShapeOf(arrivals []otlpArrival, cutoff time.Time) TelemetryShape {
	shape := TelemetryShape{Surfaces: map[string]*SurfaceShape{}}
	protocols := map[string]string{}
	spanSurface := map[string]string{} // span id → surface
	type spanRef struct{ surface, parent string }
	var spans []spanRef
	lags := map[string][]time.Duration{}
	batches := map[string][]time.Time{} // when each signal's batches before the cutoff arrived
	var metricArrivals []time.Time
	var firstPeriod time.Duration
	for _, a := range arrivals {
		if a.Payload == nil {
			continue
		}
		own := false // the arrival holds something of the build's
		before := a.At.Before(cutoff)
		switch m := a.Payload.(type) {
		case *collogspb.ExportLogsServiceRequest:
			var oldest uint64
			for _, rl := range m.GetResourceLogs() {
				if termaResource(rl.GetResource().GetAttributes()) {
					continue
				}
				for _, sl := range rl.GetScopeLogs() {
					for _, lr := range sl.GetLogRecords() {
						own = true
						ts := lr.GetTimeUnixNano()
						if ts == 0 {
							ts = lr.GetObservedTimeUnixNano()
						}
						if ts != 0 && (oldest == 0 || ts < oldest) {
							oldest = ts
						}
					}
				}
			}
			if own && before && oldest != 0 {
				lags["logs"] = append(lags["logs"], a.At.Sub(time.Unix(0, int64(oldest))))
				batches["logs"] = append(batches["logs"], a.At)
			}
		case *coltracepb.ExportTraceServiceRequest:
			var oldest uint64
			for _, rs := range m.GetResourceSpans() {
				if termaResource(rs.GetResource().GetAttributes()) {
					continue
				}
				for _, ss := range rs.GetScopeSpans() {
					for _, sp := range ss.GetSpans() {
						own = true
						surface := "traces/" + SpanSurface(sp.GetName(), sp.GetAttributes())
						spanSurface[string(sp.GetSpanId())] = surface
						spans = append(spans, spanRef{surface, string(sp.GetParentSpanId())})
						if end := sp.GetEndTimeUnixNano(); end != 0 && (oldest == 0 || end < oldest) {
							oldest = end
						}
					}
				}
			}
			if own && before && oldest != 0 {
				lags["traces"] = append(lags["traces"], a.At.Sub(time.Unix(0, int64(oldest))))
				batches["traces"] = append(batches["traces"], a.At)
			}
		case *colmetricspb.ExportMetricsServiceRequest:
			var period time.Duration
			for _, rm := range m.GetResourceMetrics() {
				if termaResource(rm.GetResource().GetAttributes()) {
					continue
				}
				for _, sm := range rm.GetScopeMetrics() {
					for _, mt := range sm.GetMetrics() {
						own = true
						s := metricShape(mt)
						shape.Surfaces["metrics/"+mt.GetName()] = s
						if p := metricPeriod(mt); p > period {
							period = p
						}
					}
				}
			}
			if own && before {
				if len(metricArrivals) == 0 {
					firstPeriod = period
				}
				metricArrivals = append(metricArrivals, a.At)
			}
		}
		if own {
			if p := otlpProtocol(a.ContentType); p != "" {
				protocols[a.Signal] = p
			}
			if a.Encoding != "" {
				shape.Export.Compression = a.Encoding
			}
		}
	}
	for _, s := range spans {
		ss := shape.Surfaces[s.surface]
		if ss == nil {
			ss = &SurfaceShape{}
			shape.Surfaces[s.surface] = ss
		}
		switch parent, ok := spanSurface[s.parent]; {
		case s.parent == "":
			ss.Root = true
		case ok && !slices.Contains(ss.Parents, parent):
			ss.Parents = append(ss.Parents, parent)
		}
	}
	for _, ss := range shape.Surfaces {
		sort.Strings(ss.Parents)
		if len(ss.Parents) == 1 {
			ss.Parent, ss.Parents = ss.Parents[0], nil
		}
	}
	// A processor that exports on a fixed tick has its oldest record wait up to a period, one
	// that starts its timer at the first record a period exactly: the longest wait is the period,
	// unless a slow export stretched it, which the shortest gap between two batches bounds.
	intervals := map[string]int64{}
	for signal, ls := range lags {
		intervals[signal] = roundInterval(slices.Max(ls))
		times := batches[signal]
		for i := 1; i < len(times); i++ {
			if gap := roundInterval(times[i].Sub(times[i-1])); gap > 0 && gap < intervals[signal] {
				intervals[signal] = gap
			}
		}
	}
	if firstPeriod > 0 {
		intervals["metrics"] = roundInterval(firstPeriod)
	}
	for i := 1; i < len(metricArrivals); i++ {
		if gap := metricArrivals[i].Sub(metricArrivals[i-1]); intervals["metrics"] == 0 || roundInterval(gap) < intervals["metrics"] {
			intervals["metrics"] = roundInterval(gap)
		}
	}
	if len(intervals) > 0 {
		shape.Export.IntervalMS = intervals
		shape.Export.IntervalBatches = map[string]int{}
		for signal := range intervals {
			shape.Export.IntervalBatches[signal] = len(batches[signal])
		}
		if intervals["metrics"] != 0 {
			shape.Export.IntervalBatches["metrics"] = len(metricArrivals)
		}
	}
	var distinct []string
	for _, p := range protocols {
		if !slices.Contains(distinct, p) {
			distinct = append(distinct, p)
		}
	}
	switch len(distinct) {
	case 0:
	case 1:
		shape.Export.Protocol = distinct[0]
	default:
		shape.Export.Protocols = protocols
	}
	return shape
}

func termaResource(attrs []*commonpb.KeyValue) bool {
	for _, kv := range attrs {
		if kv.GetKey() == "service.name" && TermaService(kv.GetValue().GetStringValue()) {
			return true
		}
	}
	return false
}

// metricShape is a metric's type, unit and temporality.
func metricShape(mt *metricspb.Metric) *SurfaceShape {
	s := &SurfaceShape{Unit: mt.GetUnit()}
	temporality := func(t metricspb.AggregationTemporality) string {
		switch t {
		case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
			return "delta"
		case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
			return "cumulative"
		}
		return ""
	}
	switch d := mt.GetData().(type) {
	case *metricspb.Metric_Sum:
		mono := d.Sum.GetIsMonotonic()
		s.Type, s.Temporality, s.Monotonic = "sum", temporality(d.Sum.GetAggregationTemporality()), &mono
	case *metricspb.Metric_Gauge:
		s.Type = "gauge"
	case *metricspb.Metric_Histogram:
		s.Type, s.Temporality = "histogram", temporality(d.Histogram.GetAggregationTemporality())
	case *metricspb.Metric_ExponentialHistogram:
		s.Type, s.Temporality = "exponential_histogram", temporality(d.ExponentialHistogram.GetAggregationTemporality())
	case *metricspb.Metric_Summary:
		s.Type = "summary"
	}
	return s
}

// metricPeriod is the longest collection period a metric's points cover: from their start to
// their time.
func metricPeriod(mt *metricspb.Metric) time.Duration {
	var longest time.Duration
	see := func(start, end uint64) {
		if start != 0 && end > start {
			longest = max(longest, time.Duration(end-start))
		}
	}
	for _, p := range mt.GetSum().GetDataPoints() {
		see(p.GetStartTimeUnixNano(), p.GetTimeUnixNano())
	}
	for _, p := range mt.GetHistogram().GetDataPoints() {
		see(p.GetStartTimeUnixNano(), p.GetTimeUnixNano())
	}
	for _, p := range mt.GetExponentialHistogram().GetDataPoints() {
		see(p.GetStartTimeUnixNano(), p.GetTimeUnixNano())
	}
	return longest
}

// otlpProtocol names an export's protocol by its content type.
func otlpProtocol(contentType string) string {
	switch {
	case strings.Contains(contentType, "protobuf"):
		return "http/protobuf"
	case strings.Contains(contentType, "json"):
		return "http/json"
	}
	return ""
}

// roundIntervals are the values a measured interval is rounded to, in milliseconds.
var roundIntervals = []int64{100, 200, 250, 500, 1000, 2000, 2500, 5000, 10000, 15000, 20000, 30000, 60000, 120000, 300000}

// roundInterval is d's nearest round value, nearest by ratio.
func roundInterval(d time.Duration) int64 {
	ms := float64(d.Milliseconds())
	if ms <= 0 {
		return 0
	}
	best, bestDist := roundIntervals[0], math.Inf(1)
	for _, v := range roundIntervals {
		if dist := math.Abs(math.Log(ms / float64(v))); dist < bestDist {
			best, bestDist = v, dist
		}
	}
	return best
}

// HoldFirst holds the first request isModelCall picks for hold before next answers it, so a
// short turn's process lives long enough for each exporter's periodic export, and notes when the
// last model call was answered (Answered).
type HoldFirst struct {
	held     sync.Once
	mu       sync.Mutex
	answered time.Time
}

// Wrap is next behind the hold.
func (h *HoldFirst) Wrap(hold time.Duration, isModelCall func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := isModelCall(r)
		if model {
			h.held.Do(func() {
				select {
				case <-time.After(hold):
				case <-r.Context().Done():
				}
			})
		}
		next.ServeHTTP(w, r)
		if model {
			h.mu.Lock()
			h.answered = time.Now().UTC()
			h.mu.Unlock()
		}
	})
}

// Answered is when the last model call was answered.
func (h *HoldFirst) Answered() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.answered
}
