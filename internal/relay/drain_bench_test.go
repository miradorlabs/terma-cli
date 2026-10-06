package relay

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// drainBodies is one small part of each signal, as an agent's exports interleave them.
func drainBodies(tb testing.TB) map[Signal][]byte {
	tb.Helper()
	msgs := map[Signal]proto.Message{
		Logs: logsOf("S", 1),
		Traces: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "turn", TraceId: make([]byte, 16), SpanId: make([]byte, 8)}}}}}}},
		Metrics: &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: &resourcepb.Resource{},
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "tokens", Data: &metricspb.Metric_Gauge{
				Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{Value: &metricspb.NumberDataPoint_AsInt{AsInt: 1}}}}}}}}}}}},
	}
	out := map[Signal][]byte{}
	for sig, m := range msgs {
		b, err := proto.Marshal(m)
		if err != nil {
			tb.Fatal(err)
		}
		out[sig] = b
	}
	return out
}

// drainOutbox queues n one-record parts on one route, the signals interleaved as global mode
// files them, then times a relay delivering them all to a host that answers at once.
func drainOutbox(b *testing.B, n int) time.Duration {
	b.Helper()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer host.Close()
	dir := b.TempDir()
	o := outbox{dir: dir}
	bodies := drainBodies(b)
	rt := routeOf(claim.Claim{ProjectID: "p1"})
	signals := []Signal{Logs, Traces, Metrics}
	start := time.Now()
	for i := range n {
		sig := signals[i%len(signals)]
		if err := o.put(rt, claim.Claim{}.Repository, newEntry(start.Add(time.Duration(i)), sig, 1), bodies[sig]); err != nil {
			b.Fatal(err)
		}
	}
	pol := Policy{Endpoint: host.URL, Key: "k", IncludePrompts: true, IncludeToolContent: true}
	r := newRelay(Options{Dir: dir, Token: token, Hold: time.Minute, Resolve: func(claim.Claim) (Policy, error) { return pol, nil }})
	b.StartTimer()
	began := time.Now()
	runRelay(b, r)
	for {
		c := r.Stats().Snapshot().Counters
		if c["forwarded.logs"]+c["forwarded.traces"]+c["forwarded.metrics"] >= n {
			break
		}
		if time.Since(began) > 5*time.Minute {
			b.Fatalf("drained %d of %d in 5 minutes", c["forwarded.logs"]+c["forwarded.traces"]+c["forwarded.metrics"], n)
		}
		time.Sleep(time.Millisecond)
	}
	took := time.Since(began)
	b.StopTimer()
	return took
}

// BenchmarkOutboxDrain is how fast a backed-up route drains: it should stay roughly flat as
// the queue grows, not fall with it.
func BenchmarkOutboxDrain(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("parts=%d", n), func(b *testing.B) {
			b.StopTimer()
			var total time.Duration
			for range b.N {
				total += drainOutbox(b, n)
			}
			b.ReportMetric(float64(n*b.N)/total.Seconds(), "parts/s")
		})
	}
}
