package relay

import (
	"errors"
	"slices"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// ContentFilter applies the relay's content withholding rules to OTLP payloads directly
// (for example, during `terma history upload`, which bypasses the live relay daemon).
type ContentFilter struct {
	rules *rules
}

// NewContentFilter builds a ContentFilter from every registered agent's capture rules.
func NewContentFilter(capturers []shape.Capturer) *ContentFilter {
	return &ContentFilter{rules: compose(nil, capturers)}
}

// FilterLogs applies prompts and toolContent withholding to msg in place using the same
// rules as the live relay, preserving safe operational metadata such as
// terma.session.historical. It returns the number of changed records/resources and any
// unclassified keys dropped.
func (f *ContentFilter) FilterLogs(msg *logspb.LogsData, prompts, toolContent bool) (int, map[string]int) {
	if f == nil || f.rules == nil || msg == nil {
		return 0, nil
	}
	unclassified := map[string]int{}
	changed := f.rules.withhold(&part{signal: Logs, msg: msg}, prompts, toolContent, unclassified)
	if len(unclassified) == 0 {
		return changed, nil
	}
	return changed, unclassified
}

// CheckoutInfo carries the repository and checkout metadata stamped onto historical logs,
// matching the fields live claims carry into stampCheckout.
type CheckoutInfo struct {
	Remote   string
	Origin   string
	Branch   string
	Root     string
	Cwd      string
	Worktree string
}

// StampOptions describes the terma resource stamps applied to an uploaded OTLP log batch.
type StampOptions struct {
	ProjectID          string
	Version            string
	UserID             string
	Checkout           CheckoutInfo
	IncludeToolContent bool
}

// StampLogs stamps project, version, checkout, and optional user attributes onto every
// resource in msg using the same rules as the live relay. Run after FilterLogs so stamped
// keys reflect the destination policy and never rely on agent-supplied values.
func StampLogs(msg *logspb.LogsData, opts StampOptions) {
	if msg == nil {
		return
	}
	for _, rl := range msg.GetResourceLogs() {
		if rl.Resource == nil {
			rl.Resource = &resourcepb.Resource{}
		}
	}
	p := &part{signal: Logs, msg: msg}
	if opts.ProjectID != "" {
		stamp(p, semconv.MiradorProjectIDKey, opts.ProjectID)
	}
	if opts.Version != "" {
		stamp(p, semconv.TermaVersionKey, opts.Version)
	}
	ran := claim.Claim{
		Remote:   opts.Checkout.Remote,
		Branch:   opts.Checkout.Branch,
		Root:     opts.Checkout.Root,
		Cwd:      opts.Checkout.Cwd,
		Worktree: opts.Checkout.Worktree,
	}
	ran.Repository.Origin = opts.Checkout.Origin
	stampCheckout(p, ran, opts.IncludeToolContent, "")
	unstamp(p, semconv.TermaUserIDKey)
	if opts.UserID != "" {
		stamp(p, semconv.TermaUserIDKey, opts.UserID)
	}
}

// ParsePartialSuccess validates and reads an OTLP ExportLogsServiceResponse protobuf body
// (field 1 partial_success: field 1 rejected_log_records varint, field 2 error_message
// string) without pulling gRPC dependencies. An empty body is a valid zero-rejection
// response; malformed or non-protobuf bytes (such as proxy HTML) return an error.
func ParsePartialSuccess(body []byte) (rejected int64, errMsg string, err error) {
	for b := body; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, "", errors.New("malformed OTLP protobuf response")
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			ps, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return 0, "", errors.New("malformed OTLP partial_success payload")
			}
			b = b[m:]
			for len(ps) > 0 {
				pnum, ptyp, pn := protowire.ConsumeTag(ps)
				if pn < 0 {
					return 0, "", errors.New("malformed OTLP partial_success tag")
				}
				ps = ps[pn:]
				switch {
				case pnum == 1 && ptyp == protowire.VarintType:
					v, pm := protowire.ConsumeVarint(ps)
					if pm < 0 {
						return 0, "", errors.New("malformed OTLP rejected_log_records")
					}
					rejected = max(0, int64(v))
					ps = ps[pm:]
				case pnum == 2 && ptyp == protowire.BytesType:
					v, pm := protowire.ConsumeString(ps)
					if pm < 0 {
						return 0, "", errors.New("malformed OTLP error_message")
					}
					errMsg = v
					ps = ps[pm:]
				default:
					pm := protowire.ConsumeFieldValue(pnum, ptyp, ps)
					if pm < 0 {
						return 0, "", errors.New("malformed OTLP partial_success field")
					}
					ps = ps[pm:]
				}
			}
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return 0, "", errors.New("malformed OTLP protobuf field")
		}
		b = b[m:]
	}
	return rejected, errMsg, nil
}

// stripHistorical removes terma.session.historical from live relay exports: only
// `terma history upload` (which bypasses the live relay daemon) sets the historical flag,
// so live agent telemetry can never spoof historical sessions.
func stripHistorical(p *part) {
	unstamp(p, semconv.TermaSessionHistoricalKey)
	dropKey := func(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
		return slices.DeleteFunc(attrs, func(kv *commonpb.KeyValue) bool {
			return kv.GetKey() == semconv.TermaSessionHistoricalKey
		})
	}
	dropExemplars := func(exs []*metricspb.Exemplar) {
		for _, ex := range exs {
			ex.FilteredAttributes = dropKey(ex.FilteredAttributes)
		}
	}
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				if sl.Scope != nil {
					sl.Scope.Attributes = dropKey(sl.Scope.Attributes)
				}
				for _, lr := range sl.GetLogRecords() {
					lr.Attributes = dropKey(lr.Attributes)
				}
			}
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				if ss.Scope != nil {
					ss.Scope.Attributes = dropKey(ss.Scope.Attributes)
				}
				for _, sp := range ss.GetSpans() {
					sp.Attributes = dropKey(sp.Attributes)
					for _, ev := range sp.GetEvents() {
						ev.Attributes = dropKey(ev.Attributes)
					}
					for _, lk := range sp.GetLinks() {
						lk.Attributes = dropKey(lk.Attributes)
					}
				}
			}
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				if sm.Scope != nil {
					sm.Scope.Attributes = dropKey(sm.Scope.Attributes)
				}
				for _, mt := range sm.GetMetrics() {
					for _, pt := range mt.GetSum().GetDataPoints() {
						pt.Attributes = dropKey(pt.Attributes)
						dropExemplars(pt.GetExemplars())
					}
					for _, pt := range mt.GetGauge().GetDataPoints() {
						pt.Attributes = dropKey(pt.Attributes)
						dropExemplars(pt.GetExemplars())
					}
					for _, pt := range mt.GetHistogram().GetDataPoints() {
						pt.Attributes = dropKey(pt.Attributes)
						dropExemplars(pt.GetExemplars())
					}
					for _, pt := range mt.GetExponentialHistogram().GetDataPoints() {
						pt.Attributes = dropKey(pt.Attributes)
						dropExemplars(pt.GetExemplars())
					}
					for _, pt := range mt.GetSummary().GetDataPoints() {
						pt.Attributes = dropKey(pt.Attributes)
					}
				}
			}
		}
	}
}
