package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// signal is one of the three OTLP/HTTP signals, named as in its path (/v1/<signal>).
type signal string

const (
	sigLogs    signal = "logs"
	sigTraces  signal = "traces"
	sigMetrics signal = "metrics"
)

func (s signal) path() string { return "/v1/" + string(s) }

func signalOfPath(path string) (signal, bool) {
	for _, s := range []signal{sigLogs, sigTraces, sigMetrics} {
		if path == s.path() {
			return s, true
		}
	}
	return "", false
}

// shape names a signal's three levels of nesting in OTLP/JSON: resources hold scopes,
// scopes hold units (a log record, a span, a metric).
type shape struct{ resources, scopes, units string }

func (s signal) shape() shape {
	switch s {
	case sigLogs:
		return shape{"resourceLogs", "scopeLogs", "logRecords"}
	case sigTraces:
		return shape{"resourceSpans", "scopeSpans", "spans"}
	default:
		return shape{"resourceMetrics", "scopeMetrics", "metrics"}
	}
}

// metricData are the fields a metric carries its data points under; exactly one is set.
var metricData = []string{"sum", "gauge", "histogram", "exponentialHistogram", "summary"}

// object is a JSON object whose values are kept as they came, so a record the relay
// splits out is forwarded byte-for-byte in every field it does not route by.
type object map[string]json.RawMessage

// batch is one OTLP/JSON request body, parsed only as deep as routing needs.
type batch struct {
	sig       signal
	resources []resourceGroup
}

type resourceGroup struct {
	fields  object // everything but the scopes array
	session string // a session named on the resource itself, applied to its items
	scopes  []scopeGroup
}

type scopeGroup struct {
	fields object // everything but the units array
	units  []unit
}

// unit is one log record, span or metric. A metric is split further, by data point,
// because each point names its own session (Claude Code) or none (Codex).
type unit struct {
	raw     json.RawMessage // logs and spans: the record itself
	fields  object          // metrics: the metric without its data points
	dataKey string          // metrics: which of metricData holds them
	data    object          // metrics: that field without its dataPoints
	items   []*item
}

// item is the smallest thing routed: a log record, a span, or a metric data point.
type item struct {
	point   json.RawMessage // metrics only
	at      time.Time       // when it happened, by the agent's clock; zero when unstated
	session string
	source  string // "claude" or "codex", by the attribute that named the session
	traceID string
	route   string
}

// errNotOTLP is returned for a body that is not an OTLP/JSON document of its signal.
var errNotOTLP = errors.New("not an OTLP/JSON body")

// parseBatch reads body as OTLP/JSON of sig.
func parseBatch(sig signal, body []byte) (*batch, error) {
	sh := sig.shape()
	var top object
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("%w: %w", errNotOTLP, err)
	}
	b := &batch{sig: sig}
	var resources []object
	if raw, ok := top[sh.resources]; ok {
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errNotOTLP, sh.resources, err)
		}
	}
	for _, res := range resources {
		rg := resourceGroup{fields: object{}}
		for k, v := range res {
			if k != sh.scopes {
				rg.fields[k] = v
			}
		}
		if raw, ok := res["resource"]; ok {
			var r struct {
				Attributes []kv `json:"attributes"`
			}
			if json.Unmarshal(raw, &r) == nil {
				rg.session, _ = sessionFrom(r.Attributes)
			}
		}
		var scopes []object
		if raw, ok := res[sh.scopes]; ok {
			if err := json.Unmarshal(raw, &scopes); err != nil {
				return nil, fmt.Errorf("%w: %s: %w", errNotOTLP, sh.scopes, err)
			}
		}
		for _, sc := range scopes {
			sg := scopeGroup{fields: object{}}
			for k, v := range sc {
				if k != sh.units {
					sg.fields[k] = v
				}
			}
			var units []json.RawMessage
			if raw, ok := sc[sh.units]; ok {
				if err := json.Unmarshal(raw, &units); err != nil {
					return nil, fmt.Errorf("%w: %s: %w", errNotOTLP, sh.units, err)
				}
			}
			for _, raw := range units {
				u, err := parseUnit(sig, raw, rg.session)
				if err != nil {
					return nil, err
				}
				sg.units = append(sg.units, u)
			}
			rg.scopes = append(rg.scopes, sg)
		}
		b.resources = append(b.resources, rg)
	}
	return b, nil
}

// record is the part of a log record, span or data point routing reads.
type record struct {
	TraceID      string          `json:"traceId"`
	Attributes   []kv            `json:"attributes"`
	Start        json.RawMessage `json:"startTimeUnixNano"`
	Time         json.RawMessage `json:"timeUnixNano"`
	ObservedTime json.RawMessage `json:"observedTimeUnixNano"`
}

// at is when the record happened: a span's or data point's start (a data point starts
// with its process, so a resumed run's points start in that run), else its time, else
// when it was observed. OTLP/JSON writes these as strings; a number is taken too.
func (r record) at() time.Time {
	for _, raw := range []json.RawMessage{r.Start, r.Time, r.ObservedTime} {
		s := strings.Trim(string(raw), `"`)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return time.Unix(0, n)
		}
	}
	return time.Time{}
}

func parseUnit(sig signal, raw json.RawMessage, resourceSession string) (unit, error) {
	if sig != sigMetrics {
		var r record
		if err := json.Unmarshal(raw, &r); err != nil {
			return unit{}, fmt.Errorf("%w: %w", errNotOTLP, err)
		}
		it := &item{traceID: r.TraceID, at: r.at()}
		it.session, it.source = sessionFrom(r.Attributes)
		if it.session == "" {
			it.session = resourceSession
		}
		return unit{raw: raw, items: []*item{it}}, nil
	}
	var metric object
	if err := json.Unmarshal(raw, &metric); err != nil {
		return unit{}, fmt.Errorf("%w: %w", errNotOTLP, err)
	}
	u := unit{fields: object{}}
	maps.Copy(u.fields, metric)
	for _, key := range metricData {
		rawData, ok := metric[key]
		if !ok {
			continue
		}
		var data object
		if err := json.Unmarshal(rawData, &data); err != nil {
			return unit{}, fmt.Errorf("%w: %s: %w", errNotOTLP, key, err)
		}
		var points []json.RawMessage
		if rawPoints, ok := data["dataPoints"]; ok {
			if err := json.Unmarshal(rawPoints, &points); err != nil {
				return unit{}, fmt.Errorf("%w: dataPoints: %w", errNotOTLP, err)
			}
		}
		delete(u.fields, key)
		delete(data, "dataPoints")
		u.dataKey, u.data = key, data
		for _, p := range points {
			var r record
			if err := json.Unmarshal(p, &r); err != nil {
				return unit{}, fmt.Errorf("%w: data point: %w", errNotOTLP, err)
			}
			it := &item{point: p, at: r.at()}
			it.session, it.source = sessionFrom(r.Attributes)
			if it.session == "" {
				it.session = resourceSession
			}
			u.items = append(u.items, it)
		}
		break
	}
	if u.dataKey == "" {
		// A metric with no data we recognise still travels, as one item of no session.
		u = unit{raw: raw, items: []*item{{session: resourceSession}}}
	}
	return u, nil
}

// items lists every routable item in the batch, in document order.
func (b *batch) items() []*item {
	var all []*item
	for _, rg := range b.resources {
		for _, sg := range rg.scopes {
			for _, u := range sg.units {
				all = append(all, u.items...)
			}
		}
	}
	return all
}

// encode writes the part of the batch whose items are routed to route, and how many
// items that is. Resources and scopes left empty are omitted; everything else is kept.
func (b *batch) encode(route string) ([]byte, int, error) {
	sh := b.sig.shape()
	var resources []object
	count := 0
	for _, rg := range b.resources {
		var scopes []object
		for _, sg := range rg.scopes {
			var units []json.RawMessage
			for _, u := range sg.units {
				raw, n, err := u.encode(route)
				if err != nil {
					return nil, 0, err
				}
				if n > 0 {
					units = append(units, raw)
					count += n
				}
			}
			if len(units) == 0 {
				continue
			}
			sc, err := withArray(sg.fields, sh.units, units)
			if err != nil {
				return nil, 0, err
			}
			scopes = append(scopes, sc)
		}
		if len(scopes) == 0 {
			continue
		}
		res, err := withArray(rg.fields, sh.scopes, scopes)
		if err != nil {
			return nil, 0, err
		}
		resources = append(resources, res)
	}
	if count == 0 {
		return nil, 0, nil
	}
	body, err := marshal(map[string]any{sh.resources: resources})
	return body, count, err
}

func (u unit) encode(route string) (json.RawMessage, int, error) {
	if u.dataKey == "" {
		if u.items[0].route != route {
			return nil, 0, nil
		}
		return u.raw, 1, nil
	}
	var points []json.RawMessage
	for _, it := range u.items {
		if it.route == route {
			points = append(points, it.point)
		}
	}
	if len(points) == 0 {
		return nil, 0, nil
	}
	data, err := withArray(u.data, "dataPoints", points)
	if err != nil {
		return nil, 0, err
	}
	rawData, err := marshal(data)
	if err != nil {
		return nil, 0, err
	}
	metric := object{}
	maps.Copy(metric, u.fields)
	metric[u.dataKey] = rawData
	raw, err := marshal(metric)
	return raw, len(points), err
}

// withArray is fields plus key set to arr.
func withArray[T any](fields object, key string, arr []T) (object, error) {
	raw, err := marshal(arr)
	if err != nil {
		return nil, err
	}
	out := make(object, len(fields)+1)
	maps.Copy(out, fields)
	out[key] = raw
	return out, nil
}

// mergeBodies concatenates OTLP/JSON bodies of one signal into one request, so a
// backlog is sent in a few requests rather than one per accepted export.
func mergeBodies(sig signal, bodies [][]byte) ([]byte, error) {
	if len(bodies) == 1 {
		return bodies[0], nil
	}
	sh := sig.shape()
	var all []json.RawMessage
	for _, body := range bodies {
		var top struct {
			Logs    []json.RawMessage `json:"resourceLogs"`
			Spans   []json.RawMessage `json:"resourceSpans"`
			Metrics []json.RawMessage `json:"resourceMetrics"`
		}
		if err := json.Unmarshal(body, &top); err != nil {
			return nil, fmt.Errorf("%w: %w", errNotOTLP, err)
		}
		all = append(all, top.Logs...)
		all = append(all, top.Spans...)
		all = append(all, top.Metrics...)
	}
	return marshal(map[string]any{sh.resources: all})
}

// kv is an OTLP attribute, read for its string value only.
type kv struct {
	Key   string `json:"key"`
	Value struct {
		String *string `json:"stringValue"`
	} `json:"value"`
}

// sessionFrom finds the session an item belongs to among its attributes, and which
// agent's convention named it. Claude Code puts session.id on every record, span and
// data point. Codex puts conversation.id on its log records and, on a few spans, the
// thread's UUID as thread.id / thread_id — most spans carry a worker-thread number
// there, which is not a session and is ignored.
func sessionFrom(attrs []kv) (session, source string) {
	for _, a := range attrs {
		if a.Value.String == nil {
			continue
		}
		v := *a.Value.String
		switch a.Key {
		case "session.id":
			if validSessionID(v) {
				return v, "claude"
			}
		case "conversation.id":
			if validSessionID(v) {
				return v, "codex"
			}
		case "thread.id", "thread_id":
			if isUUID(v) {
				return v, "codex"
			}
		}
	}
	return "", ""
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHex(r) {
				return false
			}
		}
	}
	return true
}

func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

// looksLikeJSON reports whether body starts like a JSON object, for an export whose
// content type was not given.
func looksLikeJSON(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// marshal is json.Marshal without HTML escaping: records leave as the agents wrote them,
// "<REDACTED>" as itself and not as "\u003cREDACTED\u003e". Escaping applies even to a
// json.RawMessage the encoder copies, so every re-encoding in the relay goes through here.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
