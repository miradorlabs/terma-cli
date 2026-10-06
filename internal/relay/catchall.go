package relay

import (
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func hasCatchAll(p *part) bool {
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		for _, res := range m.ResourceLogs {
			if attrString(res.GetResource().GetAttributes(), semconv.TermaRelayAttributionKey) == semconv.TermaRelayAttributionCatchAll {
				return true
			}
		}
	case *metricspb.MetricsData:
		for _, res := range m.ResourceMetrics {
			if attrString(res.GetResource().GetAttributes(), semconv.TermaRelayAttributionKey) == semconv.TermaRelayAttributionCatchAll {
				return true
			}
		}
	case *tracepb.TracesData:
		for _, res := range m.ResourceSpans {
			if attrString(res.GetResource().GetAttributes(), semconv.TermaRelayAttributionKey) == semconv.TermaRelayAttributionCatchAll {
				return true
			}
		}
	}
	return false
}
