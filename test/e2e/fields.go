package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// The field census: every attribute key each harness build emits over OTLP, on each surface,
// with the kinds of value it carried and what terma's relay does with it. Scenarios that
// export straight to the receiver report what reached it (ObserveFields); TestMain writes the
// run's rows to report/fields.json, classified by the terma under test (`terma relay
// classify`), and compatgen merges them into the field catalog (docs/compat/fields.json),
// says what changed since (report/drift.md), and renders docs/FIELDS.md.

// A surface is where a key was seen: "resource", "scope" (the instrumentation scope's
// attributes), "logs/<event>", "traces/<span>", "traces/<span>/events/<event>",
// "traces/<span>/links", "metrics/<metric>" or "metrics/<metric>/exemplars": everywhere the
// relay applies the content policy.

// Value kinds. The relay keeps an unclassified key whose every value is a number or a
// boolean, so only "text" makes an unclassified key a loss.
const (
	KindText   = "text"
	KindNumber = "number"
	KindBool   = "bool"
	KindList   = "list"
	KindMap    = "map"
	KindBytes  = "bytes"
)

// FieldRow is one key one harness build emitted on one surface, in one run.
type FieldRow struct {
	Harness  string    `json:"harness"`
	Version  string    `json:"version"`
	Surface  string    `json:"surface"`
	Key      string    `json:"key"`
	Kinds    []string  `json:"kinds"`
	Class    string    `json:"class,omitempty"` // safe | prompt | tool_content | unclassified
	Platform string    `json:"platform"`
	Terma    string    `json:"terma,omitempty"`
	At       time.Time `json:"at"`
}

type fieldID struct{ harness, version, surface, key string }

var (
	fieldsMu  sync.Mutex
	fieldSeen = map[fieldID]map[string]bool{}
)

// ObserveFields records the keys in requests, what harness b exported straight to the
// receiver. terma's own records (its hooks' delivery, the relay's heartbeat) are not b's.
func ObserveFields(b Binary, requests []ExportRequest) {
	fieldsMu.Lock()
	defer fieldsMu.Unlock()
	see := func(surface string, kvs []*commonpb.KeyValue) {
		for _, kv := range kvs {
			id := fieldID{b.Harness, b.Version, surface, kv.GetKey()}
			if fieldSeen[id] == nil {
				fieldSeen[id] = map[string]bool{}
			}
			fieldSeen[id][kindOf(kv.GetValue())] = true
		}
	}
	resource := func(r *resourcepb.Resource) bool {
		for _, kv := range r.GetAttributes() {
			if kv.GetKey() == "service.name" && TermaService(kv.GetValue().GetStringValue()) {
				return false
			}
		}
		see("resource", r.GetAttributes())
		return true
	}
	for _, req := range requests {
		switch m := req.Payload.(type) {
		case *collogspb.ExportLogsServiceRequest:
			for _, rl := range m.GetResourceLogs() {
				if !resource(rl.GetResource()) {
					continue
				}
				for _, sl := range rl.GetScopeLogs() {
					see("scope", sl.GetScope().GetAttributes())
					for _, lr := range sl.GetLogRecords() {
						// The event.name attribute is the name the platform reads; Codex puts its
						// tracing callsite in the record's EventName ("event otel/src/...rs:785").
						event := ""
						for _, kv := range lr.GetAttributes() {
							if kv.GetKey() == "event.name" {
								event = kv.GetValue().GetStringValue()
							}
						}
						if event == "" {
							event = withoutLine(lr.GetEventName())
						}
						if event == "" {
							event = "(scope " + sl.GetScope().GetName() + ")"
						}
						see("logs/"+event, lr.GetAttributes())
					}
				}
			}
		case *coltracepb.ExportTraceServiceRequest:
			for _, rs := range m.GetResourceSpans() {
				if !resource(rs.GetResource()) {
					continue
				}
				for _, ss := range rs.GetScopeSpans() {
					see("scope", ss.GetScope().GetAttributes())
					for _, sp := range ss.GetSpans() {
						name := SpanSurface(sp.GetName(), sp.GetAttributes())
						see("traces/"+name, sp.GetAttributes())
						for _, ev := range sp.GetEvents() {
							see("traces/"+name+"/events/"+withoutLine(ev.GetName()), ev.GetAttributes())
						}
						for _, l := range sp.GetLinks() {
							see("traces/"+name+"/links", l.GetAttributes())
						}
					}
				}
			}
		case *colmetricspb.ExportMetricsServiceRequest:
			for _, rm := range m.GetResourceMetrics() {
				if !resource(rm.GetResource()) {
					continue
				}
				for _, sm := range rm.GetScopeMetrics() {
					see("scope", sm.GetScope().GetAttributes())
					for _, mt := range sm.GetMetrics() {
						surface := "metrics/" + mt.GetName()
						exemplars := func(es []*metricspb.Exemplar) {
							for _, e := range es {
								see(surface+"/exemplars", e.GetFilteredAttributes())
							}
						}
						for _, p := range mt.GetSum().GetDataPoints() {
							see(surface, p.GetAttributes())
							exemplars(p.GetExemplars())
						}
						for _, p := range mt.GetGauge().GetDataPoints() {
							see(surface, p.GetAttributes())
							exemplars(p.GetExemplars())
						}
						for _, p := range mt.GetHistogram().GetDataPoints() {
							see(surface, p.GetAttributes())
							exemplars(p.GetExemplars())
						}
						for _, p := range mt.GetExponentialHistogram().GetDataPoints() {
							see(surface, p.GetAttributes())
							exemplars(p.GetExemplars())
						}
						for _, p := range mt.GetSummary().GetDataPoints() {
							see(surface, p.GetAttributes())
						}
					}
				}
			}
		}
	}
}

// TermaService reports a service.name of terma's own records, not an agent's: its hooks'
// delivery and the relay's heartbeat.
func TermaService(name string) bool { return name == "terma-cli" || name == "terma-relay" }

// SpanSurface names a span's surface: a GenAI span is named "<operation> <target>" (OpenCode's
// "chat <model>", "execute_tool <tool>"), so its target becomes "{target}"; a name ending in a
// source line loses it (withoutLine); other names stay.
func SpanSurface(name string, attrs []*commonpb.KeyValue) string {
	for _, kv := range attrs {
		if kv.GetKey() == "gen_ai.operation.name" {
			if op := kv.GetValue().GetStringValue(); op != "" && strings.HasPrefix(name, op+" ") {
				return op + " {target}"
			}
		}
	}
	return withoutLine(name)
}

// withoutLine drops a trailing ":<line>": Codex names its span events by the source line that
// logged them ("event otel/src/tool_result.rs:54"), which moves with every release.
func withoutLine(name string) string {
	i := strings.LastIndexByte(name, ':')
	if i < 0 || i == len(name)-1 {
		return name
	}
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return name
		}
	}
	return name[:i]
}

func kindOf(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_IntValue, *commonpb.AnyValue_DoubleValue:
		return KindNumber
	case *commonpb.AnyValue_BoolValue:
		return KindBool
	case *commonpb.AnyValue_ArrayValue:
		return KindList
	case *commonpb.AnyValue_KvlistValue:
		return KindMap
	case *commonpb.AnyValue_BytesValue:
		return KindBytes
	case *commonpb.AnyValue_StringValue:
		// The relay counts a string that is wholly a number or a boolean as one: the rule is
		// internal/relay's numericOrBool, kept the same here.
		s := x.StringValue
		if s == "true" || s == "false" {
			return KindBool
		}
		if s == "" || len(s) > 32 {
			return KindText
		}
		if _, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "xXpPiInN_") {
			return KindNumber
		}
	}
	return KindText
}

// WriteFields writes this run's census to dir/fields.json, each key classified by the terma
// at path, as its relay would treat it. A census it cannot classify is not written: the
// nightly digest then says the census is missing, where unclassified rows would read as a
// quiet night.
func WriteFields(dir, terma string) error {
	fieldsMu.Lock()
	rows := make([]FieldRow, 0, len(fieldSeen))
	now := time.Now().UTC()
	platform := runtime.GOOS + "/" + runtime.GOARCH
	keys := map[string]bool{}
	for id, kinds := range fieldSeen {
		rows = append(rows, FieldRow{Harness: id.harness, Version: id.version, Surface: id.surface, Key: id.key,
			Kinds: slices.Sorted(maps.Keys(kinds)), Platform: platform, Terma: Version(terma), At: now})
		keys[classKey(id.surface, id.key)] = true
	}
	fieldsMu.Unlock()
	// No census from an earlier run stays for `make drift` to take for this one's, whether this
	// one took none or could not classify it.
	path := filepath.Join(dir, "fields.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	classes, err := classify(terma, slices.Sorted(maps.Keys(keys)))
	if err != nil {
		return fmt.Errorf("classify the census with %s: %w", terma, err)
	}
	for i := range rows {
		c, ok := classes[classKey(rows[i].Surface, rows[i].Key)]
		if !ok {
			return fmt.Errorf("%s classified no %q", terma, rows[i].Key)
		}
		rows[i].Class = c
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return a.Harness+"\x00"+a.Version+"\x00"+a.Surface+"\x00"+a.Key < b.Harness+"\x00"+b.Version+"\x00"+b.Surface+"\x00"+b.Key
	})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// classKey is the key as `terma relay classify` takes it: a resource attribute's and a span
// event's prefixed, since the relay treats each by where it is.
func classKey(surface, key string) string {
	if surface == "resource" {
		return "resource/" + key
	}
	if _, event, ok := strings.Cut(surface, "/events/"); ok && strings.HasPrefix(surface, "traces/") {
		return "event/" + event + "/" + key
	}
	return key
}

// classify asks the terma at path what its relay does with each key.
func classify(terma string, keys []string) (map[string]string, error) {
	in, _ := json.Marshal(keys)
	cmd := exec.Command(terma, "relay", "classify")
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	classes := map[string]string{}
	return classes, json.Unmarshal(out, &classes)
}
