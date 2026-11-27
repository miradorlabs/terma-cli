package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// The replay harness: testdata/replay/driver.mjs runs the rendered extension under Node
// against one recorded Pi process (testdata/replay/<version>/<case>.<n>.jsonl, real events
// recorded on that Pi build), and this file parses what it sent: every post to the relay and
// every `terma hook` call, in order.

// nodeForTest is a Node that runs TypeScript, or the test is skipped.
func nodeForTest(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	// The extension is TypeScript, which only a Node that strips types loads (22.18+, 23.6+).
	if out, err := exec.Command(node, "-p", "process.features.typescript").Output(); err != nil || !slices.Contains([]string{"strip", "transform"}, strings.TrimSpace(string(out))) {
		t.Skip("node does not run TypeScript")
	}
	return node
}

func recording(version, name string) string {
	return filepath.Join("testdata", "replay", version, name+".jsonl")
}

type replayOptions struct {
	// Host is the fake Pi module's exports; nil is Pi as recorded (its VERSION, ".pi").
	Host map[string]any
	// NoHost leaves Pi's module out.
	NoHost bool
	// Modules are other bare modules that resolve beside the extension.
	Modules []string
	// FailPosts answer 503 before the relay answers 200.
	FailPosts int
	// Parent is the TERMA_PI_PARENT the process inherits.
	Parent string
	// FailReach makes every pi-tool-reach fail but the fence's own.
	FailReach bool
	// NoPrompts and NoToolContent switch content off in the extension's CONFIG.
	NoPrompts, NoToolContent bool
	// Inert writes the raw template, whose CONFIG is null.
	Inert bool
}

type hookCall struct {
	Event   string
	Payload map[string]any
}

type post struct {
	Path   string `json:"path"`
	Body   string `json:"body"`
	Status int    `json:"status"`
}

type replayed struct {
	PID        int      `json:"pid"`
	Registered int      `json:"registered"`
	Idle       bool     `json:"idle"`
	Errors     []string `json:"errors"`
	EnvParent  *string  `json:"envParent"`
	Posts      []post   `json:"posts"`
	Hooks      []hookCall
	Spans      []otlpSpan
	Records    []otlpRecord
}

func replay(t *testing.T, path string, o replayOptions) *replayed {
	t.Helper()
	node := nodeForTest(t)
	driver, err := filepath.Abs(filepath.Join("testdata", "replay", "driver.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	hookLog := filepath.Join(dir, "hooks.log")
	hook := filepath.Join(dir, "hook.sh")
	hookSrc := `#!/bin/sh
p=$(cat)
printf '%s\t%s\n' "$1" "$p" >> "$TERMA_TEST_HOOK_LOG"
if [ -n "$TERMA_TEST_FAIL_REACH" ] && [ "$1" = "pi-tool-reach" ]; then
  case "$p" in *TERMA_REACH_FAILED*) exit 0 ;; esac
  exit 1
fi
exit 0
`
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(dir, "ext", "terma.ts")
	if err := os.MkdirAll(filepath.Dir(ext), 0o700); err != nil {
		t.Fatal(err)
	}
	text := extensionTemplate
	if !o.Inert {
		text, err = renderExtension(extensionConfig{
			Endpoint:           "http://127.0.0.1:43180",
			Headers:            map[string]string{"Authorization": "Bearer relay-token"},
			IncludePrompts:     !o.NoPrompts,
			IncludeToolContent: !o.NoToolContent,
			HookCommand:        []string{hook},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ext, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	host := o.Host
	if host == nil {
		host = map[string]any{"VERSION": nil, "CONFIG_DIR_NAME": ".pi"}
	}
	opts := map[string]any{"ext": ext, "recording": rec, "out": filepath.Join(dir, "out"), "host": host, "modules": o.Modules, "failPosts": o.FailPosts}
	if o.NoHost {
		opts["host"] = nil
	}
	arg, _ := json.Marshal(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, driver, string(arg))
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || k == "TERMA_PI_PARENT" || strings.HasPrefix(k, "TERMA_TEST_") {
			continue
		}
		env = append(env, kv)
	}
	// Pi's ~ is the home directory the recordings were scrubbed to.
	env = append(env, "HOME=/home/user", "TERMA_TEST_HOOK_LOG="+hookLog)
	if o.Parent != "" {
		env = append(env, "TERMA_PI_PARENT="+o.Parent)
	}
	if o.FailReach {
		env = append(env, "TERMA_TEST_FAIL_REACH=1")
	}
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r replayed
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if !r.Idle {
		t.Errorf("the extension's queues did not drain")
	}
	if len(r.Errors) > 0 {
		t.Errorf("handlers: %v", r.Errors)
	}
	if f, err := os.Open(hookLog); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			event, payload, _ := strings.Cut(sc.Text(), "\t")
			var p map[string]any
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				t.Fatalf("hook %s payload %q: %v", event, payload, err)
			}
			r.Hooks = append(r.Hooks, hookCall{Event: event, Payload: p})
		}
		_ = f.Close()
	}
	for _, p := range r.Posts {
		if p.Status != 200 {
			continue
		}
		spans, records := parseOTLP(t, p)
		r.Spans = append(r.Spans, spans...)
		r.Records = append(r.Records, records...)
	}
	return &r
}

// --- OTLP/JSON ---

type otlpValue struct {
	StringValue *string    `json:"stringValue"`
	IntValue    *string    `json:"intValue"`
	DoubleValue *float64   `json:"doubleValue"`
	BoolValue   *bool      `json:"boolValue"`
	ArrayValue  *otlpArray `json:"arrayValue"`
}

type otlpArray struct {
	Values []otlpValue `json:"values"`
}

type otlpKV struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpAttrs map[string]otlpValue

func attrsOf(kvs []otlpKV) otlpAttrs {
	a := otlpAttrs{}
	for _, kv := range kvs {
		a[kv.Key] = kv.Value
	}
	return a
}

func (a otlpAttrs) has(k string) bool { _, ok := a[k]; return ok }

func (a otlpAttrs) str(k string) string {
	if v, ok := a[k]; ok && v.StringValue != nil {
		return *v.StringValue
	}
	return ""
}

func (a otlpAttrs) int(k string) (int64, bool) {
	if v, ok := a[k]; ok && v.IntValue != nil {
		n, err := strconv.ParseInt(*v.IntValue, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (a otlpAttrs) boolean(k string) (bool, bool) {
	if v, ok := a[k]; ok && v.BoolValue != nil {
		return *v.BoolValue, true
	}
	return false, false
}

func (a otlpAttrs) double(k string) (float64, bool) {
	if v, ok := a[k]; ok && v.DoubleValue != nil {
		return *v.DoubleValue, true
	}
	return 0, false
}

func (a otlpAttrs) strs(k string) []string {
	v, ok := a[k]
	if !ok || v.ArrayValue == nil {
		return nil
	}
	var out []string
	for _, x := range v.ArrayValue.Values {
		if x.StringValue != nil {
			out = append(out, *x.StringValue)
		}
	}
	return out
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type otlpSpan struct {
	TraceID, SpanID, Name string
	Kind                  int
	Start, End            int64
	Attrs                 otlpAttrs
	Status                int
	Scope                 otlpScope
	Resource              otlpAttrs
}

type otlpRecord struct {
	Time, Observed int64
	Body           string
	Attrs          otlpAttrs
	TraceID        string
	Scope          otlpScope
	Resource       otlpAttrs
}

func nanosOf(t *testing.T, s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return n
}

func parseOTLP(t *testing.T, p post) ([]otlpSpan, []otlpRecord) {
	t.Helper()
	var spans []otlpSpan
	var records []otlpRecord
	switch p.Path {
	case "/v1/traces":
		var body struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []otlpKV `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Scope otlpScope `json:"scope"`
					Spans []struct {
						TraceID    string   `json:"traceId"`
						SpanID     string   `json:"spanId"`
						Name       string   `json:"name"`
						Kind       int      `json:"kind"`
						Start      string   `json:"startTimeUnixNano"`
						End        string   `json:"endTimeUnixNano"`
						Attributes []otlpKV `json:"attributes"`
						Status     struct {
							Code int `json:"code"`
						} `json:"status"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal([]byte(p.Body), &body); err != nil {
			t.Fatalf("traces: %v", err)
		}
		for _, rs := range body.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					spans = append(spans, otlpSpan{TraceID: sp.TraceID, SpanID: sp.SpanID, Name: sp.Name, Kind: sp.Kind,
						Start: nanosOf(t, sp.Start), End: nanosOf(t, sp.End), Attrs: attrsOf(sp.Attributes), Status: sp.Status.Code,
						Scope: ss.Scope, Resource: attrsOf(rs.Resource.Attributes)})
				}
			}
		}
	case "/v1/logs":
		var body struct {
			ResourceLogs []struct {
				Resource struct {
					Attributes []otlpKV `json:"attributes"`
				} `json:"resource"`
				ScopeLogs []struct {
					Scope      otlpScope `json:"scope"`
					LogRecords []struct {
						Time     string `json:"timeUnixNano"`
						Observed string `json:"observedTimeUnixNano"`
						Body     struct {
							StringValue string `json:"stringValue"`
						} `json:"body"`
						Attributes []otlpKV `json:"attributes"`
						TraceID    string   `json:"traceId"`
					} `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if err := json.Unmarshal([]byte(p.Body), &body); err != nil {
			t.Fatalf("logs: %v", err)
		}
		for _, rl := range body.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					records = append(records, otlpRecord{Time: nanosOf(t, lr.Time), Observed: nanosOf(t, lr.Observed), Body: lr.Body.StringValue,
						Attrs: attrsOf(lr.Attributes), TraceID: lr.TraceID, Scope: sl.Scope, Resource: attrsOf(rl.Resource.Attributes)})
				}
			}
		}
	default:
		t.Fatalf("post to %s", p.Path)
	}
	return spans, records
}

// --- what was sent ---

func (r *replayed) chats() []otlpSpan {
	var out []otlpSpan
	for _, s := range r.Spans {
		if s.Attrs.str("gen_ai.operation.name") == "chat" {
			out = append(out, s)
		}
	}
	return out
}

// calls are the conversation's model calls: chat spans of no other kind.
func (r *replayed) calls() []otlpSpan {
	var out []otlpSpan
	for _, s := range r.chats() {
		if !s.Attrs.has("pi.call.kind") {
			out = append(out, s)
		}
	}
	return out
}

func (r *replayed) tools() []otlpSpan {
	var out []otlpSpan
	for _, s := range r.Spans {
		if s.Attrs.str("gen_ai.operation.name") == "execute_tool" {
			out = append(out, s)
		}
	}
	return out
}

func (r *replayed) tool(name string) []otlpSpan {
	var out []otlpSpan
	for _, s := range r.tools() {
		if s.Attrs.str("gen_ai.tool.name") == name {
			out = append(out, s)
		}
	}
	return out
}

func (r *replayed) records(event string) []otlpRecord {
	var out []otlpRecord
	for _, rec := range r.Records {
		if rec.Attrs.str("event.name") == event {
			out = append(out, rec)
		}
	}
	return out
}

func (r *replayed) hooks(event string) []map[string]any {
	var out []map[string]any
	for _, h := range r.Hooks {
		if h.Event == event {
			out = append(out, h.Payload)
		}
	}
	return out
}

// everyAttrs are the attributes of every span and record sent.
func (r *replayed) everyAttrs() []otlpAttrs {
	var out []otlpAttrs
	for _, s := range r.Spans {
		out = append(out, s.Attrs)
	}
	for _, rec := range r.Records {
		out = append(out, rec.Attrs)
	}
	return out
}

// ids are every id a span or record was sent under: span ids, call ids, entry ids, tool
// call ids.
func (r *replayed) ids() []string {
	var out []string
	for _, s := range r.Spans {
		out = append(out, "span:"+s.SpanID)
	}
	for _, a := range r.everyAttrs() {
		for _, k := range []string{"pi.call.id", "pi.entry.id", "gen_ai.tool.call.id"} {
			// A tool span names the call that issued it; only a chat span is that call.
			if k == "pi.call.id" && a.str("gen_ai.operation.name") != "chat" {
				continue
			}
			if v := a.str(k); v != "" {
				out = append(out, a.str("session.id")+"/"+k+":"+v)
			}
		}
	}
	return out
}

func (r *replayed) allBodies() string {
	var b strings.Builder
	for _, p := range r.Posts {
		b.WriteString(p.Body)
	}
	return b.String()
}

// --- recordings ---

type line = map[string]any

func loadLines(t *testing.T, path string) []line {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []line
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m line
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func writeLines(t *testing.T, lines []line) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	var b strings.Builder
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// get walks a decoded JSON value by keys.
func get(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func gstr(v any, keys ...string) string {
	s, _ := get(v, keys...).(string)
	return s
}

func gnum(v any, keys ...string) float64 {
	n, _ := get(v, keys...).(float64)
	return n
}

func events(lines []line, name string) []line {
	var out []line
	for _, l := range lines {
		if l["e"] == name {
			out = append(out, l)
		}
	}
	return out
}

// entries are every entry the recording lists, in order, with the session it was listed for.
type listed struct {
	Session string
	Entry   map[string]any
}

func entries(lines []line) []listed {
	var out []listed
	for _, l := range lines {
		if l["e"] != "_entries" {
			continue
		}
		list, _ := l["entries"].([]any)
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				out = append(out, listed{Session: gstr(l, "sessionId"), Entry: m})
			}
		}
	}
	return out
}

// entriesBefore are the ids of the entries a session holds when its event at index i fires.
func entriesBefore(lines []line, session string, i int) map[string]bool {
	out := map[string]bool{}
	for _, l := range lines[:i] {
		if l["e"] == "_entries" && gstr(l, "sessionId") == session {
			for _, e := range l["entries"].([]any) {
				out[gstr(e, "id")] = true
			}
		}
	}
	return out
}

func indexOf(lines []line, pred func(line) bool) int {
	for i, l := range lines {
		if pred(l) {
			return i
		}
	}
	return -1
}

// jsLen is a string's length as JavaScript counts it, in UTF-16 code units.
func jsLen(s string) int64 { return int64(len(utf16.Encode([]rune(s)))) }

func textOfContent(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	var parts []string
	list, _ := c.([]any)
	for _, p := range list {
		if gstr(p, "type") == "text" {
			parts = append(parts, gstr(p, "text"))
		}
	}
	return strings.Join(parts, "\n")
}

func ms(n float64) int64 { return int64(n) * 1_000_000 }

func isoNanos(t *testing.T, s string) int64 {
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return tm.UnixMilli() * 1_000_000
}

func sessionIDs(lines []line) []string {
	var out []string
	for _, l := range events(lines, "_session") {
		id := gstr(l, "sessionId")
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return out
}
