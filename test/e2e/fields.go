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
	"testing"
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

// Value kinds, as `terma relay classify` names them in the kinds a key keeps. Where an
// unclassified key keeps any, on a record, they are numbers and booleans: what cannot carry
// what was said.
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
	Harness string   `json:"harness"`
	Version string   `json:"version"`
	Surface string   `json:"surface"`
	Key     string   `json:"key"`
	Kinds   []string `json:"kinds"`
	Class   string   `json:"class,omitempty"` // safe | prompt | tool_content | unclassified
	// Kept are the kinds of value the relay keeps of an unclassified key where it sits, as
	// `terma relay classify` says: on a record numbers and booleans, on a resource none.
	Kept     []string  `json:"kept,omitempty"`
	Platform string    `json:"platform"`
	Terma    string    `json:"terma,omitempty"`
	At       time.Time `json:"at"`
}

// Withheld reports an unclassified key the relay drops under a policy that withholds
// content: one sent with a kind of value the relay does not keep where it sits.
func Withheld(class string, kinds, kept []string) bool {
	return class == "unclassified" && slices.ContainsFunc(kinds, func(k string) bool { return !slices.Contains(kept, k) })
}

// CensusRun is a build a scenario that takes the census (the test that calls runBoth, and
// TestClaudeInteractiveFields) ran, and whether it failed for it. A build's census is whole
// only if every census scenario that ran that night ran it and none failed: one a scenario
// failed for, or did not run, is partial. A build they ran and no census reached was not
// reached. report/census.json holds them, beside the census.
type CensusRun struct {
	Harness  string `json:"harness"`
	Version  string `json:"version"`
	Scenario string `json:"scenario"`
	Failed   bool   `json:"failed,omitempty"`
	Platform string `json:"platform"`
}

var censusRuns = map[[3]string]*CensusRun{}

// TakesCensus records that t takes the census of b: if t fails, b's census is partial. A
// skipped t ran nothing.
func TakesCensus(t *testing.T, b Binary) {
	t.Helper()
	t.Cleanup(func() {
		if t.Skipped() && !t.Failed() {
			return
		}
		fieldsMu.Lock()
		defer fieldsMu.Unlock()
		scenario, _, _ := strings.Cut(t.Name(), "/")
		k := [3]string{b.Harness, b.Version, scenario}
		if censusRuns[k] == nil {
			censusRuns[k] = &CensusRun{Harness: b.Harness, Version: b.Version, Scenario: scenario, Platform: runtime.GOOS + "/" + runtime.GOARCH}
		}
		censusRuns[k].Failed = censusRuns[k].Failed || t.Failed()
	})
}

type fieldID struct{ harness, version, surface, key string }

// seenField is what the census saw of a key: the kinds of value it carried, and where it sits
// as `terma relay classify` asks it.
type seenField struct {
	kinds map[string]bool
	query classQuery
}

var (
	fieldsMu  sync.Mutex
	fieldSeen = map[fieldID]*seenField{}
)

// ObserveFields records the keys in requests, what harness b exported straight to the
// receiver. terma's own records (its hooks' delivery, the relay's heartbeat) are not b's.
func ObserveFields(b Binary, requests []ExportRequest) {
	fieldsMu.Lock()
	defer fieldsMu.Unlock()
	// seeAt records the keys in kvs on surface, sitting at site, on the span event named event
	// for one there; see records a record's, or what the policy treats as one.
	seeAt := func(site, event, surface string, kvs []*commonpb.KeyValue) {
		for _, kv := range kvs {
			id := fieldID{b.Harness, b.Version, surface, kv.GetKey()}
			if fieldSeen[id] == nil {
				fieldSeen[id] = &seenField{kinds: map[string]bool{}, query: classQuery{Site: site, Event: event, Key: kv.GetKey()}}
			}
			fieldSeen[id].kinds[kindOf(kv.GetValue())] = true
		}
	}
	see := func(surface string, kvs []*commonpb.KeyValue) { seeAt("record", "", surface, kvs) }
	resource := func(r *resourcepb.Resource) bool {
		for _, kv := range r.GetAttributes() {
			if kv.GetKey() == "service.name" && TermaService(kv.GetValue().GetStringValue()) {
				return false
			}
		}
		seeAt("resource", "", "resource", r.GetAttributes())
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
							event := withoutLine(ev.GetName())
							seeAt("event", event, "traces/"+name+"/events/"+event, ev.GetAttributes())
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
	platform, version := runtime.GOOS+"/"+runtime.GOARCH, Version(terma)
	queries := map[classQuery]bool{}
	asked := make([]classQuery, 0, len(fieldSeen))
	for id, f := range fieldSeen {
		rows = append(rows, FieldRow{Harness: id.harness, Version: id.version, Surface: id.surface, Key: id.key,
			Kinds: slices.Sorted(maps.Keys(f.kinds)), Platform: platform, Terma: version, At: now})
		asked = append(asked, f.query)
		queries[f.query] = true
	}
	runs := make([]CensusRun, 0, len(censusRuns))
	for _, r := range censusRuns {
		runs = append(runs, *r)
	}
	fieldsMu.Unlock()
	// No census from an earlier run stays for `make drift` to take for this one's, whether this
	// one took none or could not classify it. What the census scenarios ran is written either
	// way: it says the census is missing, or partial.
	path := filepath.Join(dir, "fields.json")
	for _, p := range []string{path, filepath.Join(dir, "census.json")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if len(runs) > 0 {
		sort.Slice(runs, func(i, j int) bool {
			a, b := runs[i], runs[j]
			return a.Harness+"\x00"+a.Version+"\x00"+a.Scenario < b.Harness+"\x00"+b.Version+"\x00"+b.Scenario
		})
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		data, err := json.MarshalIndent(runs, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "census.json"), append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		return nil
	}
	verdicts, err := classify(terma, slices.Collect(maps.Keys(queries)))
	if err != nil {
		return fmt.Errorf("classify the census with %s: %w", terma, err)
	}
	for i := range rows {
		v := verdicts[asked[i]]
		rows[i].Class = v.Class
		if v.Class == "unclassified" {
			rows[i].Kept = v.Kept
		}
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

// classQuery is a key as `terma relay classify` takes it: where it sits, as the relay's
// content policy tells them apart (a resource, a span event, or a record, as it treats every
// other surface), and the span event's name. The census records it as it sees the key, so no
// surface's name is read back for it.
type classQuery struct {
	Site  string `json:"site"`
	Event string `json:"event,omitempty"`
	Key   string `json:"key"`
}

// verdict is what `terma relay classify` says of a key: its class, and the kinds of value it
// keeps with all content withheld.
type verdict struct {
	Class string   `json:"class"`
	Kept  []string `json:"kept"`
}

// classify asks the terma at path what its relay does with each key.
func classify(terma string, queries []classQuery) (map[classQuery]verdict, error) {
	in, err := json.Marshal(queries)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(terma, "relay", "classify")
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var verdicts []verdict
	if err := json.Unmarshal(out, &verdicts); err != nil {
		return nil, err
	}
	if len(verdicts) != len(queries) {
		return nil, fmt.Errorf("%d verdicts for %d keys", len(verdicts), len(queries))
	}
	byQuery := make(map[classQuery]verdict, len(queries))
	for i, q := range queries {
		byQuery[q] = verdicts[i]
	}
	return byQuery, nil
}
