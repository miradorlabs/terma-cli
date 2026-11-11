package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// A fake Messages API: it refuses temperature (with a request id in its message), is always
// overloaded for top_k, answers anything else, streams, and counts tokens a word each.
func fakeAnthropic(t *testing.T) *client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		if r.Header.Get("x-api-key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			msgs := body["messages"].([]any)
			text := msgs[0].(map[string]any)["content"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": len(strings.Fields(text))})
		case body["temperature"] != nil:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"temperature is not supported for this model (request req_011abcDEF)."}}`))
		case body["top_k"] != nil:
			w.WriteHeader(529)
		case body["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message_start\ndata: {}\n\nevent: content_block_delta\ndata: {}\n\nevent: content_block_delta\ndata: {}\n\nevent: message_stop\ndata: {}\n\n"))
		default:
			w.Header().Set("Anthropic-Ratelimit-Requests-Limit", "50")
			w.Header().Set("Request-Id", "req_1")
			w.Header().Set("Cf-Ray", "x")
			_ = json.NewEncoder(w).Encode(map[string]any{"model": body["model"], "stop_reason": "max_tokens",
				"usage": map[string]any{"input_tokens": 9, "output_tokens": 16, "cache_creation": map[string]any{"ephemeral_5m_input_tokens": 0}}})
		}
	}))
	t.Cleanup(srv.Close)
	c := newClient()
	c.wait = time.Millisecond
	c.tries = 2
	c.rewrite = func(u string) string { return srv.URL + strings.TrimPrefix(u, "https://api.anthropic.com") }
	return c
}

func TestAnthropicProbe(t *testing.T) {
	c := fakeAnthropic(t)
	m := &Snapshot{ID: "claude-fable-5-2", Facts: map[string]any{factMaxOutput: int64(128_000)}}
	p := anthropicProbe(context.Background(), c, "k", m)
	if r := p.Params["baseline"]; !r.Accepted || r.Status != 200 {
		t.Errorf("baseline = %+v", r)
	}
	r := p.Params["sampling.temperature"]
	if r.Accepted || r.Status != 400 || r.Error != "temperature is not supported for this model (request <id>)." {
		t.Errorf("temperature = %+v", r)
	}
	// Never answered meaningfully: left out, not a refusal.
	if _, ok := p.Params["sampling.top_k"]; ok {
		t.Errorf("an overloaded case was recorded: %+v", p.Params["sampling.top_k"])
	}
	if !slices.ContainsFunc(p.Errors, func(e string) bool { return strings.HasPrefix(e, "sampling.top_k") }) {
		t.Errorf("errors = %v", p.Errors)
	}
	if _, ok := p.Params["max_tokens.over_stated_limit"]; !ok {
		t.Error("the output limit was not probed")
	}
	s := p.Response
	if s == nil || s.Model != "claude-fable-5-2" {
		t.Fatalf("response = %+v", s)
	}
	if !slices.Equal(s.UsageKeys, []string{"cache_creation.ephemeral_5m_input_tokens", "input_tokens", "output_tokens"}) {
		t.Errorf("usage keys = %v", s.UsageKeys)
	}
	if !slices.Equal(s.Headers, []string{"anthropic-ratelimit-requests-limit", "request-id"}) {
		t.Errorf("headers = %v", s.Headers)
	}
	if !slices.Equal(s.StreamEvents, []string{"message_start", "content_block_delta", "message_stop"}) {
		t.Errorf("stream events = %v", s.StreamEvents)
	}
	if len(p.Tokens) != len(corpus) || p.Tokens["prose"] == 0 {
		t.Errorf("tokens = %v", p.Tokens)
	}
}

func TestTokenizerLike(t *testing.T) {
	same := map[string]int{}
	for i, c := range corpus {
		same[c.name] = i + 10
	}
	other := map[string]int{}
	for i, c := range corpus {
		other[c.name] = i + 11
	}
	probed := func(id string, tokens map[string]int) *Snapshot {
		return &Snapshot{ID: id, Vendor: "anthropic", API: &API{Probes: &Probes{Tokens: tokens}}}
	}
	snaps := map[string]*Snapshot{"a": probed("a", same), "b": probed("b", same), "c": probed("c", other), "d": {ID: "d", Vendor: "anthropic"}}
	tokenizerLike(snaps)
	if got := snaps["a"].API.Probes.TokenizerLike; !slices.Equal(got, []string{"b"}) {
		t.Errorf("a like %v", got)
	}
	if got := snaps["c"].API.Probes.TokenizerLike; len(got) != 0 {
		t.Errorf("c like %v", got)
	}
}

func TestMerge(t *testing.T) {
	base := map[string]any{"model": "m", "input": "x", "cfg": map[string]any{"a": 1, "b": 2}}
	got := merge(base, map[string]any{"input": del, "cfg": map[string]any{"b": 3}})
	if _, ok := got["input"]; ok || got["cfg"].(map[string]any)["a"] != 1 || got["cfg"].(map[string]any)["b"] != 3 {
		t.Errorf("merge = %v", got)
	}
	if base["cfg"].(map[string]any)["b"] != 2 {
		t.Error("merge changed the plain request")
	}
}

// A re-probe the API partly did not answer (overloaded: the plain request, its stream, a case)
// observes nothing new: the snapshot keeps every byte, and the timeline gets no step.
func TestUnansweredProbesChangeNoByte(t *testing.T) {
	overloaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		_, extra := body["top_k"]
		plain := len(body) == 3 // model, max_tokens, messages
		switch {
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": 7})
		case overloaded && (extra || body["stream"] == true && len(body) == 4):
			w.WriteHeader(529)
		case extra:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"top_k: Extra inputs are not permitted"}}`))
		case body["stream"] == true:
			_, _ = w.Write([]byte("event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n"))
		case overloaded && plain:
			w.WriteHeader(529)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"model": body["model"], "stop_reason": "end_turn",
				"usage": map[string]any{"input_tokens": 9, "output_tokens": 2}})
		}
	}))
	t.Cleanup(srv.Close)
	c := newClient()
	c.wait, c.tries = time.Millisecond, 1
	c.rewrite = func(u string) string { return srv.URL + strings.TrimPrefix(u, "https://api.anthropic.com") }
	probe := func(s *Snapshot) *Probes { return anthropicProbe(context.Background(), c, "k", s) }
	dir := t.TempDir()
	run := func(at time.Time) []byte {
		t.Helper()
		prev, err := readPublished(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := publish(dir, testRun(at, docsObs("claude-fable-5-2", fable52)), prev, nil, nil, probe); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "claude-fable-5-2", "snapshot.json"))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := run(testNow)
	if !strings.Contains(string(first), `"stream_events"`) || !strings.Contains(string(first), `"sampling.top_k"`) {
		t.Fatalf("the first run did not observe the response, its stream and top_k:\n%s", first)
	}
	overloaded = true
	if later := run(testNow.Add(24 * time.Hour)); !bytes.Equal(first, later) {
		t.Errorf("a partly unanswered re-probe changed the snapshot:\n%s", later)
	}
	if tl := readTimeline(t, dir, "claude-fable-5-2"); len(tl.Steps) != 1 {
		t.Errorf("a partly unanswered re-probe made a step: %+v", tl.Steps[0])
	}
}

// A model the key may not use refuses the plain request: nothing else is sent, and the probes say
// why, rather than every case looking refused by the model.
func TestProbeUnavailableModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"model: claude-mythos-5-2"}}`))
	}))
	t.Cleanup(srv.Close)
	c := newClient()
	calls := 0
	c.rewrite = func(u string) string { calls++; return srv.URL + strings.TrimPrefix(u, "https://api.anthropic.com") }
	p := anthropicProbe(context.Background(), c, "k", &Snapshot{ID: "claude-mythos-5-2", Facts: map[string]any{}})
	if p.Unavailable != "404 model: claude-mythos-5-2" || len(p.Params) != 0 || p.Tokens != nil || calls != 1 {
		t.Errorf("probes = %+v after %d requests", p, calls)
	}
}

// A long refusal is cut at a character, never inside one.
func TestAPIErrorCutsAtACharacter(t *testing.T) {
	msg := apiError([]byte(`{"error":{"message":"` + strings.Repeat("é", 450) + `"}}`))
	if !utf8.ValidString(msg) || utf8.RuneCountInString(msg) != 401 {
		t.Errorf("cut to %d runes, valid %v", utf8.RuneCountInString(msg), utf8.ValidString(msg))
	}
}

// A stream the API refuses is an answer (no events), unlike one it did not answer.
func TestRefusedStreamIsObserved(t *testing.T) {
	old := &Probes{Endpoint: "e", Params: map[string]ProbeResult{"baseline": {Accepted: true, Status: 200}},
		Response: &ResponseShape{Model: "m", StreamEvents: []string{"message_start"}}}
	refused := &Probes{Endpoint: "e", Params: map[string]ProbeResult{"baseline": {Accepted: true, Status: 200}},
		Response: &ResponseShape{Model: "m"}, unanswered: map[string]bool{}}
	if got := keepUnobserved(old, refused); got.Response.StreamEvents != nil {
		t.Errorf("a refused stream kept the old events: %v", got.Response.StreamEvents)
	}
	missed := &Probes{Endpoint: "e", Params: map[string]ProbeResult{"baseline": {Accepted: true, Status: 200}},
		Response: &ResponseShape{Model: "m"}, unanswered: map[string]bool{}, streamMissed: true}
	if got := keepUnobserved(old, missed); len(got.Response.StreamEvents) != 1 {
		t.Errorf("an unanswered stream lost the old events: %v", got.Response.StreamEvents)
	}
}

func TestNeedsProbe(t *testing.T) {
	seen := &Probes{Params: map[string]ProbeResult{"baseline": {Accepted: true, Status: 200}}}
	day := testNow.Add(-24 * time.Hour)
	for _, tc := range []struct {
		name   string
		probes *Probes
		mode   string
		last   time.Time
		want   bool
	}{
		{"never probed, new", nil, "new", time.Time{}, true},
		{"every case unanswered, new", &Probes{Params: map[string]ProbeResult{}}, "new", time.Time{}, true},
		{"unavailable to the key, new", &Probes{Params: map[string]ProbeResult{}, Unavailable: "404 model"}, "new", time.Time{}, false},
		{"probed, new", seen, "new", time.Time{}, false},
		{"probed, all", seen, "all", testNow, true},
		{"never probed, none", nil, "none", time.Time{}, false},
		{"probed an hour ago, daily", seen, "daily", testNow.Add(-time.Hour), false},
		{"probed a day ago, daily", seen, "daily", day, true},
		{"probed, time unknown, daily", seen, "daily", time.Time{}, true},
		{"never probed, daily", nil, "daily", time.Time{}, true},
	} {
		s := &Snapshot{ID: "m"}
		if tc.probes != nil {
			s.API = &API{Probes: tc.probes}
		}
		if got := needsProbe(s, tc.mode, tc.last, testNow); got != tc.want {
			t.Errorf("%s: needsProbe = %v", tc.name, got)
		}
	}
}

// The stream's answer, as runProbes reads it: events where it streamed; none where the API refused
// to stream (an answer, so the events are gone); unanswered where it was overloaded (what the
// stream sent before stands).
func TestStreamAnswers(t *testing.T) {
	for _, tc := range []struct {
		status   int
		events   []string
		unanswer bool
	}{
		{200, []string{"message_start", "message_stop"}, false},
		{400, nil, false},
		{529, nil, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &body)
			if body["stream"] != true {
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "m", "usage": map[string]any{"input_tokens": 1}})
				return
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n"))
		}))
		c := newClient()
		c.wait, c.tries = time.Millisecond, 1
		target := probeTarget{url: srv.URL, base: map[string]any{"model": "m"}, cases: []probeCase{{name: "baseline"}},
			shape: shapeOf("model", "usage"),
			stream: func(base map[string]any) (string, map[string]any) {
				return srv.URL, merge(base, map[string]any{"stream": true})
			}}
		p := runProbes(context.Background(), c, target, "e", testNow)
		srv.Close()
		if !slices.Equal(p.Response.StreamEvents, tc.events) || p.streamMissed != tc.unanswer {
			t.Errorf("stream %d: events %v, unanswered %v", tc.status, p.Response.StreamEvents, p.streamMissed)
		}
	}
}

// A key the vendor refuses for its own reasons (401, out of credit) observes nothing: the probes
// before stand, every case of them. Every model of a vendor refused at once is the key's doing too.
func TestKeyRefusalsAreNoChange(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, `{"error":{"message":"invalid x-api-key"}}`},
		{400, `{"error":{"message":"Your credit balance is too low to access the Anthropic API."}}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := newClient()
		c.wait, c.tries = time.Millisecond, 1
		c.rewrite = func(u string) string { return srv.URL + strings.TrimPrefix(u, "https://api.anthropic.com") }
		p := anthropicProbe(context.Background(), c, "k", &Snapshot{ID: "m", Facts: map[string]any{}})
		srv.Close()
		old := &Probes{Endpoint: p.Endpoint, Params: map[string]ProbeResult{"baseline": {Accepted: true, Status: 200}, "effort.max": {Accepted: true, Status: 200}},
			Tokens: map[string]int{"prose": 1}, Response: &ResponseShape{Model: "m", StreamEvents: []string{"message_start"}}}
		if p.Unavailable != "" {
			t.Errorf("%d: the key's refusal read as the model's: %s", tc.status, p.Unavailable)
		}
		if got := keepUnobserved(old, p); !reflectEqual(probeView(got), probeView(old)) {
			t.Errorf("%d: the key's refusal changed the probes: %+v", tc.status, got)
		}
	}
	todo := []*Snapshot{{ID: "a", Vendor: "anthropic"}, {ID: "b", Vendor: "anthropic"}, {ID: "g", Vendor: "google"}}
	probed := []*Probes{{Unavailable: "404 x"}, {Unavailable: "404 y"}, {Unavailable: "404 z"}}
	keyWide(todo, probed)
	if probed[0] != nil || probed[1] != nil || probed[2] == nil {
		t.Errorf("after keyWide: %v %v %v (a lone unavailable model is the model's)", probed[0], probed[1], probed[2])
	}
}

// A model that comes to count tokens the same as another (a new model of its tokenizer) records
// that on its timeline; the snapshot before is not changed in the run that compares it.
func TestTokenizerLikeIsAChange(t *testing.T) {
	tokens := map[string]int{}
	for _, c := range corpus {
		tokens[c.name] = 10
	}
	probe := func(*Snapshot) *Probes {
		return &Probes{Endpoint: "e", Params: map[string]ProbeResult{"baseline": {Accepted: true, Status: 200}}, Tokens: maps.Clone(tokens)}
	}
	dir := t.TempDir()
	for i, obs := range [][]observation{
		{docsObs("claude-fable-5-1", fable51)},
		{docsObs("claude-fable-5-1", fable51), docsObs("claude-fable-5-2", fable52)},
	} {
		prev, err := readPublished(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := publish(dir, testRun(testNow.Add(time.Duration(i)*time.Hour), obs...), prev, nil, nil, probe); err != nil {
			t.Fatal(err)
		}
	}
	tl := readTimeline(t, dir, "claude-fable-5-1")
	if len(tl.Steps) != 2 || tl.Steps[0].Changes[0].Title != "Counts tokens the same as claude-fable-5-2" {
		t.Errorf("fable 5.1 timeline = %+v", tl.Steps)
	}
}

// A vendor that refuses the key once is not asked again that run: its other models wait.
func TestAKeyRefusedStopsTheVendor(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	}))
	t.Cleanup(srv.Close)
	c := newClient()
	c.wait, c.tries = time.Millisecond, 1
	c.rewrite = func(u string) string { return srv.URL + strings.TrimPrefix(u, "https://api.anthropic.com") }
	probe := func(s *Snapshot) *Probes { return anthropicProbe(context.Background(), c, "k", s) }
	var obs []observation
	for _, id := range []string{"claude-a-1", "claude-b-1", "claude-c-1", "claude-d-1", "claude-e-1", "claude-f-1", "claude-g-1", "claude-h-1"} {
		obs = append(obs, docsObs(id, map[string]any{}))
	}
	prev, _ := readPublished(t.TempDir())
	res, err := publish(t.TempDir(), testRun(testNow, obs...), prev, nil, nil, probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.observed) != 0 {
		t.Errorf("observed %v", res.observed)
	}
	// At most the models already in flight when the first refusal came back (probeParallel).
	if calls > probeParallel {
		t.Errorf("%d requests for a refused key, want at most %d", calls, probeParallel)
	}
}
