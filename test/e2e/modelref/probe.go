package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Probes: the reference's own requests to a model, on its vendor's API, with the vendor's key.
// Each case is a small request (a short prompt, a few output tokens) that differs from a plain
// one in one parameter, and what the API did with it (accepted it, or refused it and said why)
// is the case's result: so the page shows what a model's API takes, not only what its docs say,
// and its timeline shows when that changes. A fixed corpus's token counts show the tokenizer.
// A request that never reached an answer the API meant (the network, a 429, a 5xx) is not
// observed, never a refusal.

// probeCase is one request: the plain request with body merged into it (del removes a key).
type probeCase struct {
	name string
	path string // a path of its own, where it is not the vendor's plain endpoint
	body map[string]any
}

// del, as a value in a case's body, removes the key from the plain request.
type delMarker struct{}

var del = delMarker{}

// probeTarget is how one vendor is probed.
type probeTarget struct {
	url     string
	headers map[string]string
	base    map[string]any
	cases   []probeCase
	// shape reads a plain answer: the model it says answered, why it stopped, its usage, and a
	// fingerprint of the backend where the vendor sends one.
	shape func(body []byte) ResponseShape
	// stream is the plain request streamed, nil where the vendor is not probed streaming.
	stream func(base map[string]any) (url string, body map[string]any)
	// count is the token count of text, for the tokenizer corpus.
	count func(ctx context.Context, c *client, text string) (int, error)
}

// runProbes probes one model.
func runProbes(ctx context.Context, c *client, t probeTarget, endpoint string, now time.Time) *Probes {
	p := &Probes{At: now, Endpoint: endpoint, Params: map[string]ProbeResult{}, unanswered: map[string]bool{}}
	for _, pc := range t.cases {
		body := merge(t.base, pc.body)
		url := t.url
		if pc.path != "" {
			url = pc.path
		}
		resp, err := c.post(ctx, url, t.headers, body)
		if err != nil {
			p.unanswered[pc.name] = true
			p.Errors = append(p.Errors, pc.name+": not answered")
			continue
		}
		if retryable(resp.Status) {
			p.unanswered[pc.name] = true
			p.Errors = append(p.Errors, pc.name+": "+http.StatusText(resp.Status))
			continue
		}
		r := ProbeResult{Accepted: resp.Status/100 == 2, Status: resp.Status}
		if !r.Accepted {
			r.Error = apiError(resp.Body)
		}
		if pc.name == "baseline" && !r.Accepted {
			// The plain request refused (a model the key may not use, one taken down): no case
			// would say anything of the model's API.
			p.Unavailable = fmt.Sprintf("%d %s", r.Status, r.Error)
			p.Params = map[string]ProbeResult{}
			return p
		}
		p.Params[pc.name] = r
		if pc.name == "baseline" && r.Accepted && t.shape != nil {
			s := t.shape(resp.Body)
			s.Headers = headerNames(resp.Header)
			p.Response = &s
		}
	}
	if p.Response != nil && t.stream != nil {
		url, body := t.stream(t.base)
		resp, err := c.post(ctx, url, t.headers, body)
		switch {
		case err != nil || retryable(resp.Status):
			// Not answered: what the stream sent before stands.
			p.streamMissed = true
			p.Errors = append(p.Errors, "stream: not answered")
		case resp.Status/100 == 2:
			p.Response.StreamEvents = streamEvents(resp.Body)
		}
		// A refused stream is an answer: no events.
	}
	if t.count != nil {
		p.Tokens = map[string]int{}
		for _, text := range corpus {
			n, err := t.count(ctx, c, text.text)
			if err != nil {
				p.Errors = append(p.Errors, "tokens "+text.name+": "+err.Error())
				p.Tokens = nil
				break
			}
			p.Tokens[text.name] = n
		}
	}
	return p
}

// keepUnobserved is p with what this run did not observe taken from old, the model's probes
// before: a case the API did not answer, the plain answer and its stream where those were not
// read, the corpus where it was not counted. Not observed is no change; where nothing differs
// from old after that, old itself is kept, its time included, so a run that only confirms the
// probes writes the same bytes.
func keepUnobserved(old, p *Probes) *Probes {
	if old == nil {
		return p
	}
	for name := range p.unanswered {
		if r, ok := old.Params[name]; ok {
			p.Params[name] = r
		}
	}
	switch {
	case p.Response == nil && p.unanswered["baseline"]:
		p.Response = old.Response
	case p.Response != nil && p.streamMissed && old.Response != nil:
		p.Response.StreamEvents = old.Response.StreamEvents
	}
	if p.Tokens == nil {
		p.Tokens = old.Tokens
	}
	if reflectEqual(probeView(old), probeView(p)) {
		kept := *old
		kept.Errors = p.Errors
		return &kept
	}
	return p
}

// merge is base with patch laid over it, maps merged key by key.
func merge(base, patch map[string]any) map[string]any {
	out := maps.Clone(base)
	for k, v := range patch {
		if _, ok := v.(delMarker); ok {
			delete(out, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = merge(bm, pm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// apiError is the message an API's refusal gives, as the reference keeps it: without the ids
// that make one refusal's text differ from the next.
func apiError(body []byte) string {
	var v map[string]any
	msg := ""
	if json.Unmarshal(body, &v) == nil {
		switch e := v["error"].(type) {
		case map[string]any:
			msg, _ = e["message"].(string)
		case string:
			msg = e
		}
		if msg == "" {
			msg, _ = v["message"].(string)
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	msg = reRequestID.ReplaceAllString(msg, "<id>")
	if r := []rune(msg); len(r) > 400 {
		msg = string(r[:400]) + "…"
	}
	return msg
}

var reRequestID = regexp.MustCompile(`\b(?:req|msg|resp|chatcmpl)_[A-Za-z0-9]+\b|\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// headerNames are the response headers a reader of the API would read: its rate limits, its
// request ids, what it says of processing and the model. Transport headers (the CDN's, caching)
// are left out.
func headerNames(h http.Header) []string {
	var out []string
	for k := range h {
		lk := strings.ToLower(k)
		for _, s := range []string{"ratelimit", "request-id", "retry-after", "processing", "anthropic", "openai", "model", "x-goog", "x-xai", "x-grok"} {
			if strings.Contains(lk, s) {
				out = append(out, lk)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// streamEvents are the kinds of event a stream sent, each once, in the order first sent: its
// SSE event names, or the type (or object) its data carries where it names none.
func streamEvents(body []byte) []string {
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	named := false
	for sc.Scan() {
		l := sc.Text()
		if name, ok := strings.CutPrefix(l, "event:"); ok {
			add(strings.TrimSpace(name))
			named = true
			continue
		}
		data, ok := strings.CutPrefix(l, "data:")
		if !ok || named {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			add("[DONE]")
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(data), &v) == nil {
			if t, ok := v["type"].(string); ok {
				add(t)
			} else if t, ok := v["object"].(string); ok {
				add(t)
			}
		}
	}
	return out
}

// usageKeys are the keys a usage object holds, nested ones dotted.
func usageKeys(u any) []string {
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			if prefix != "" {
				out = append(out, prefix)
			}
			return
		}
		for k, x := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			walk(p, x)
		}
	}
	walk("", u)
	sort.Strings(out)
	return out
}

// tokenizerLike sets, on each probed model, the other models that count every text of the
// corpus the same.
func tokenizerLike(snaps map[string]*Snapshot) {
	type counted struct {
		id     string
		tokens map[string]int
	}
	var all []counted
	for id, s := range snaps {
		if s.API != nil && s.API.Probes != nil && len(s.API.Probes.Tokens) == len(corpus) {
			all = append(all, counted{id, s.API.Probes.Tokens})
		}
	}
	for _, a := range all {
		var like []string
		for _, b := range all {
			if a.id != b.id && snaps[a.id].Vendor == snaps[b.id].Vendor && maps.Equal(a.tokens, b.tokens) {
				like = append(like, b.id)
			}
		}
		sort.Strings(like)
		snaps[a.id].API.Probes.TokenizerLike = like
	}
}
