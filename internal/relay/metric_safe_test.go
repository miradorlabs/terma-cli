package relay

import (
	"maps"
	"slices"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// The keys an agent makes safe on one metric alone, as composed, pinned like the safe keys.
var pinnedMetricSafeKeys = map[string][]string{
	"codex.shell_snapshot.command": {"state"},
	"codex.shell_snapshot.wait_ms": {"state"},
}

func TestComposedMetricSafeKeysArePinned(t *testing.T) {
	t.Parallel()
	if !maps.EqualFunc(testRules.metricSafeKeys, pinnedMetricSafeKeys, slices.Equal) {
		t.Errorf("the composed metric safe keys are %v, pinned %v", testRules.metricSafeKeys, pinnedMetricSafeKeys)
	}
	for metric, keys := range testRules.metricSafeKeys {
		for _, k := range keys {
			if testRules.safeKey(k) {
				t.Errorf("%s is safe everywhere, so listing it under %s says nothing", k, metric)
			}
		}
	}
}

// Codex's shell snapshot state is safe on its two metrics, with content withheld, and on no
// other: a metric of another name, its exemplars excepted, drops it as unclassified.
func TestAKeyIsSafeOnItsMetricAlone(t *testing.T) {
	t.Parallel()
	point := func(metric string) (*part, *metricspb.NumberDataPoint, *metricspb.Exemplar) {
		ex := &metricspb.Exemplar{FilteredAttributes: []*commonpb.KeyValue{kv("state", "prewarm_ready")}}
		pt := &metricspb.NumberDataPoint{Attributes: []*commonpb.KeyValue{kv("state", "prewarm_ready"), kv("outcome", "success")}, Exemplars: []*metricspb.Exemplar{ex}}
		return &part{signal: Metrics, msg: &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: &resourcepb.Resource{},
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: metric,
				Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{pt}}}}}}}}}}}, pt, ex
	}
	for metric, kept := range map[string]bool{"codex.shell_snapshot.command": true, "codex.shell_snapshot.wait_ms": true, "codex.other": false} {
		p, pt, ex := point(metric)
		unclassified := map[string]int{}
		testRules.withhold(p, false, false, unclassified)
		if got := attr(pt.Attributes, "state") != ""; got != kept {
			t.Errorf("%s: state kept %v, want %v", metric, got, kept)
		}
		if got := attr(ex.FilteredAttributes, "state") != ""; got != kept {
			t.Errorf("%s: its exemplar's state kept %v, want %v", metric, got, kept)
		}
		if (unclassified["state"] > 0) == kept {
			t.Errorf("%s: unclassified %v", metric, unclassified)
		}
	}
	if v := Classify(testCapturers, []FieldQuery{
		{Site: SiteMetric, Event: "codex.shell_snapshot.command", Key: "state"},
		{Site: SiteMetric, Event: "codex.other", Key: "state"},
		{Site: SiteRecord, Key: "state"},
	}); v[0].Class != FieldSafe || v[1].Class != FieldUnclassified || v[2].Class != FieldUnclassified {
		t.Errorf("classified %+v: safe on its metric alone", v)
	}
}
