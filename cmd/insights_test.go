package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/api"
)

// runInsights executes the command tree against a fake gateway, authenticated with a
// server key so no credential file is involved, and returns what a consumer would see.
// It runs under a deadline: a read that waits on a feed must fail a test, not hang it.
func runInsights(t *testing.T, handler http.HandlerFunc, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	run := termaRun{within: 30 * time.Second, env: fakeGateway(t, handler)}
	return run.exec(t, append([]string{"-o", "json"}, args...)...)
}

// The principal catalog every insight command loads first.
const principalsJSON = `{"principals":[
	{"kind":"user","id":"u-dawson-cc","name":"dawson@mirador.org","source_system":"claude-code"},
	{"kind":"user","id":"u-dawson-cx","name":"dawson@mirador.org","source_system":"codex"},
	{"kind":"user","id":"u-dana","name":"dana@mirador.org","source_system":"claude-code","alias":"Dana"},
	{"kind":"api_key","id":"k1","name":"ci-bot","source_system":"openrouter"}
]}`

func principalsThen(t *testing.T, rest http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ai/principals" {
			fmt.Fprint(w, principalsJSON)
			return
		}
		rest(w, r)
	}
}

func TestInsightCommandTree(t *testing.T) {
	root := NewRootCommand()
	for _, path := range []string{"session list", "session get", "session events", "session git", "usage", "principal list", "principal find"} {
		fields := strings.Fields(path)
		cmd, _, err := root.Find(fields)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if leaf := fields[len(fields)-1]; cmd.Name() != leaf {
			t.Fatalf("`terma %s` resolves to %q", path, cmd.CommandPath())
		}
		if cmd.RunE == nil {
			t.Errorf("`terma %s` does nothing", path)
		}
	}
}

// "How much did Dawson use today?" — the name becomes the ids the catalog knows for
// that person, on every agent they use, and the window becomes an absolute bound on
// the session's last activity.
func TestSessionList_ResolvesNameAndWindow(t *testing.T) {
	var got url.Values
	stdout, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		got = r.URL.Query()
		fmt.Fprint(w, `{"project_id":"p","sessions":[{"session_id":"sid1","source_system":"claude-code","user_id":"u-dawson-cc","turns":4,"usage":{"input_tokens":100,"output_tokens":50,"provider_cost_usd":2.5},"models":["claude-opus-4-8"]}],"pagination":{"page":1,"per_page":100,"total":140,"total_pages":2}}`)
	}), "session", "list", "--user", "dawson", "--source", "claude-code", "--since", "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if f := got.Get("filter"); f != `source_system="claude-code" AND (user_id="u-dawson-cc" OR user_id="u-dawson-cx")` {
		t.Errorf("filter = %q", f)
	}
	// A date is the caller's local midnight; what travels is the instant.
	if at, err := time.Parse(time.RFC3339, got.Get("active_after")); err != nil || !at.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local)) {
		t.Errorf("active_after = %q, want local midnight of 2026-09-01 as RFC 3339", got.Get("active_after"))
	}
	if len(got) != 2 {
		t.Errorf("query = %v, want only filter and active_after", got)
	}

	var view sessionListView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if len(view.Sessions) != 1 || view.Sessions[0].UserName != "dawson@mirador.org" || view.Sessions[0].SessionID != "sid1" {
		t.Fatalf("view = %+v", view)
	}
	if view.Pagination == nil || view.Pagination.TotalPages != 2 || view.Pagination.Total != 140 || view.ProjectID != "p" {
		t.Fatalf("pagination = %+v project = %q", view.Pagination, view.ProjectID)
	}
	if u := view.Sessions[0].Usage; u.TotalTokens() != 150 || u.CostUSD() != 2.5 {
		t.Errorf("usage = %+v", u)
	}
}

func TestSessionList_AmbiguousNameIsAnError(t *testing.T) {
	_, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no session read should happen; got %s", r.URL.Path)
	}), "session", "list", "--user", "da")
	// Dana is named by the alias she chose, Dawson by the email the provider reported.
	if err == nil || !strings.Contains(err.Error(), "Dana") || !strings.Contains(err.Error(), "dawson@mirador.org") {
		t.Fatalf("err = %v, want both candidates named", err)
	}
}

func TestPrincipalIndexResolve(t *testing.T) {
	var page api.AIPrincipalsPage
	if err := json.Unmarshal([]byte(principalsJSON), &page); err != nil {
		t.Fatal(err)
	}
	index := newPrincipalIndex(page.Principals)
	ids := func(kind, q string) []string {
		got, err := index.resolveIDs(kind, []string{q})
		if err != nil {
			t.Fatalf("resolve %q: %v", q, err)
		}
		return got
	}
	// An exact id is one principal, even when its name is shared.
	if got := ids("user", "u-dawson-cx"); len(got) != 1 || got[0] != "u-dawson-cx" {
		t.Errorf("by id = %v", got)
	}
	// An exact email is that person on every agent, whatever the case.
	if got := ids("user", "DAWSON@mirador.org"); len(got) != 2 {
		t.Errorf("by email = %v", got)
	}
	// An alias resolves, and a unique substring resolves.
	if got := ids("user", "dana"); len(got) != 1 || got[0] != "u-dana" {
		t.Errorf("by alias = %v", got)
	}
	if got := ids("user", "wson"); len(got) != 2 {
		t.Errorf("by substring = %v", got)
	}
	// The kind is a hard boundary: a key label never resolves as a user.
	if _, err := index.resolveIDs("user", []string{"ci-bot"}); err == nil {
		t.Error("a key label resolved as a user")
	}
	if got := ids("api_key", "ci-bot"); len(got) != 1 || got[0] != "k1" {
		t.Errorf("by key label = %v", got)
	}
	// Labels come back for known ids only; unknown ids are left for the caller to print raw.
	if index.name("claude-code", "u-dana") != "Dana" || index.name("codex", "ghost") != "" {
		t.Error("name lookups")
	}
}

func TestSessionList_AllFollowsPages(t *testing.T) {
	var pages []string
	stdout, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if page == "1" {
			fmt.Fprint(w, `{"sessions":[{"session_id":"a","source_system":"codex"}],"pagination":{"page":1,"per_page":1,"total":2,"total_pages":2}}`)
			return
		}
		fmt.Fprint(w, `{"sessions":[{"session_id":"b","source_system":"codex"}],"pagination":{"page":2,"per_page":1,"total":2,"total_pages":2}}`)
	}), "session", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	var view sessionListView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil || len(view.Sessions) != 2 || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("pages=%v sessions=%d err=%v", pages, len(view.Sessions), err)
	}
	// The rows are no single page of the gateway's, so none is claimed.
	if view.Pagination != nil {
		t.Fatalf("pagination = %+v on a walk", view.Pagination)
	}
}

func TestSessionGet_NotFoundNamesTheFix(t *testing.T) {
	_, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions/summary/stream" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"NOT_FOUND","message":"session not found"}}`)
	}), "session", "get", "nope", "--source", "codex")
	if err == nil || !strings.Contains(err.Error(), "terma session list") || !strings.Contains(err.Error(), "session id") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "routing") {
		t.Fatalf("err = %v, which names an identity the gateway no longer exposes", err)
	}
}

func TestSessionEvents_FiltersAndKeepsOrder(t *testing.T) {
	stdout, _, err := runInsights(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions/events" || r.URL.Query().Get("source_system") != "codex" {
			t.Fatalf("%s %v", r.URL.Path, r.URL.Query())
		}
		fmt.Fprint(w, `{"session_id":"sid","source_system":"codex","events":[
			{"kind":"user_message","event_time":"2026-09-08T12:00:00Z","content":[{"part_index":1,"content_type":"text/plain","content":"world"},{"part_index":0,"content_type":"text/plain","content":"hello"}]},
			{"kind":"tool_call","event_time":"2026-09-08T12:00:01Z","tool_name":"Bash"},
			{"kind":"model_call","event_time":"2026-09-08T12:00:02Z","model":"gpt-5","usage":{"output_tokens":9}},
			{"kind":"tool_result","event_time":"2026-09-08T12:00:03Z","tool_name":"Bash","status":"error"}]}`)
	}, "session", "events", "sid", "--source", "codex", "--tools-only")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Events []struct {
			Kind string `json:"kind"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 2 || resp.Events[0].Kind != "tool_call" || resp.Events[1].Kind != "tool_result" {
		t.Fatalf("events = %+v", resp.Events)
	}

	_, _, err = runInsights(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("an unknown kind must be rejected before any request")
	}, "session", "events", "sid", "--source", "codex", "--kind", "nonsense")
	if err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionGitFollow_EmitsEnvelopesAndReportsRotation(t *testing.T) {
	stdout, _, err := runInsights(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/git-activities/stream" || r.URL.Query().Get("session_id") != "sid" {
			t.Fatalf("%s %v", r.URL.Path, r.URL.Query())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "retry: 3000\n\n"+
			"event: upsert\ndata: {\"activity\":{\"activity_id\":\"a1\",\"action\":\"commit\",\"outcome\":\"attempted\",\"event_time\":\"2026-09-08T12:00:00Z\"}}\n\n"+
			"event: heartbeat\ndata: {\"time\":\"2026-09-08T12:00:05Z\"}\n\n"+
			"event: snapshot_completed\ndata: {}\n\n"+
			"event: upsert\ndata: {\"activity\":{\"activity_id\":\"a1\",\"action\":\"commit\",\"outcome\":\"confirmed\",\"event_time\":\"2026-09-08T12:00:00Z\"}}\n\n")
	}, "session", "git", "sid", "--source", "claude-code", "--follow")
	if err == nil || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("the hourly rotation must surface as an error, got %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("frames:\n%s", stdout)
	}
	for i, want := range []string{"upsert", "snapshot_completed", "upsert"} {
		var env struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &env); err != nil || env.Event != want {
			t.Fatalf("frame %d = %s (%v)", i, lines[i], err)
		}
	}
	if !strings.Contains(lines[2], `"confirmed"`) {
		t.Fatalf("the later upsert must carry the newer state: %s", lines[2])
	}
}

// The usage report is six PromQL queries over one window, joined by group and
// labelled with names from the catalog.
func TestUsage_BuildsPromQLAndJoinsNames(t *testing.T) {
	var queries []string
	stdout, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics/query" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		if r.URL.Query().Get("time") != "2026-09-09T00:00:00Z" {
			t.Errorf("time = %q", r.URL.Query().Get("time"))
		}
		value := "0"
		switch {
		case strings.Contains(q, "cost.usd.total"):
			value = "1.25"
		case strings.Contains(q, "tokens.input.total"):
			value = "1000.4"
		case strings.Contains(q, "tokens.output.total"):
			value = "200"
		case strings.Contains(q, "model_call.total"):
			value = "7"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"source_system":"claude-code","user_id":"u-dawson-cc"},"value":[1757376000,"%s"]},
			{"metric":{"source_system":"codex","user_id":"u-dawson-cx"},"value":[1757376000,"0"]}]}}`, value)
	}), "usage", "--user", "Dawson", "--since", "2026-09-08T00:00:00Z", "--until", "2026-09-09T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 6 {
		t.Fatalf("queries = %d:\n%s", len(queries), strings.Join(queries, "\n"))
	}
	want := `sum by (source_system, user_id) (increase({__name__="terma.ai.cost.usd.total", user_id=~"u-dawson-cc|u-dawson-cx"}[86400s]))`
	if queries[0] != want {
		t.Fatalf("query[0] =\n%s\nwant\n%s", queries[0], want)
	}

	var report usageReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	// The codex row came back at zero on every counter: that person was idle there, and
	// an idle row must not be reported as "used, cost nothing".
	if report.Basis != "metrics_window" || report.GroupBy != "user" || len(report.Rows) != 1 {
		t.Fatalf("report = %+v", report)
	}
	top := report.Rows[0]
	if top.Name != "dawson@mirador.org" || top.Group["source_system"] != "claude-code" {
		t.Fatalf("top row = %+v", top)
	}
	if top.CostUSD != 1.25 || top.InputTokens != 1000 || top.OutputTokens != 200 || top.TotalTokens != 1200 || top.ModelCalls != 7 {
		t.Fatalf("top row numbers = %+v", top)
	}
	if report.Totals.TotalTokens != 1200 || report.Totals.CostUSD != 1.25 {
		t.Fatalf("totals = %+v", report.Totals)
	}
	if got := report.Filters["user_id"]; len(got) != 2 {
		t.Fatalf("filters = %v", report.Filters)
	}
}

func TestUsage_GroupByNoneAndRejectsUnknownGroup(t *testing.T) {
	stdout, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if !strings.HasPrefix(q, "sum(increase(") || strings.Contains(q, "sum by") {
			t.Errorf("query = %s", q)
		}
		if !strings.Contains(q, `source_system="codex"`) || !strings.Contains(q, `model=~"a\\.1|b"`) {
			t.Errorf("matchers missing: %s", q)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"3"]}]}}`)
	}), "usage", "--group-by", "none", "--source", "codex", "--model", "a.1", "--model", "b", "--since", "2h")
	if err != nil {
		t.Fatal(err)
	}
	var report usageReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || len(report.Rows) != 1 || report.Rows[0].ModelCalls != 3 {
		t.Fatalf("report = %+v err=%v", report, err)
	}

	_, _, err = runInsights(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("no request expected")
	}, "usage", "--group-by", "team")
	if err == nil || !strings.Contains(err.Error(), "--group-by") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrincipalFind(t *testing.T) {
	stdout, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected %s", r.URL.Path)
	}), "principal", "find", "dana")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"u-dana"`) || strings.Contains(stdout, "u-dawson") {
		t.Fatalf("stdout = %s", stdout)
	}
	_, _, err = runInsights(t, principalsThen(t, nil), "principal", "find", "nobody")
	if err == nil || !strings.Contains(err.Error(), "terma principal list") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseTimeArg(t *testing.T) {
	loc := time.FixedZone("test", -5*3600)
	now := time.Date(2026, 9, 9, 15, 30, 0, 0, loc)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"now", now},
		{"today", time.Date(2026, 9, 9, 0, 0, 0, 0, loc)},
		{"yesterday", time.Date(2026, 9, 8, 0, 0, 0, 0, loc)},
		{"2026-09-01", time.Date(2026, 9, 1, 0, 0, 0, 0, loc)},
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"90s", now.Add(-90 * time.Second)},
		{"2026-09-09T10:00:00Z", time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)},
		{" Today ", time.Date(2026, 9, 9, 0, 0, 0, 0, loc)},
	}
	for _, tc := range cases {
		got, err := parseTimeArg(tc.in, now)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("parseTimeArg(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "tomorrow", "-5m", "0d", "2026-13-01", "5 days"} {
		if _, err := parseTimeArg(bad, now); err == nil {
			t.Errorf("parseTimeArg(%q) accepted", bad)
		}
	}
}

func TestResolveWindow(t *testing.T) {
	now := time.Date(2026, 9, 9, 15, 30, 0, 0, time.UTC)
	w, err := resolveWindow("", "", 24*time.Hour, now)
	if err != nil || !w.until.Equal(now) || !w.since.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("default window = %+v %v", w, err)
	}
	w, err = resolveWindow("", "", 0, now)
	if err != nil || !w.since.IsZero() {
		t.Fatalf("unbounded window = %+v %v", w, err)
	}
	if _, err := resolveWindow("today", "yesterday", 0, now); err == nil {
		t.Fatal("an inverted window must be rejected")
	}
	if _, err := resolveWindow("soon", "", 0, now); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionSelectorFilter(t *testing.T) {
	s := sessionSelector{
		sources:   []string{"claude-code", "codex", "codex"},
		userIDs:   []string{`u"1`},
		models:    []string{"opus"},
		providers: []string{"anthropic", "openai"},
		extra:     `api_key_id="k"`,
	}
	want := `(source_system="claude-code" OR source_system="codex") AND user_id="u\"1" AND model:"opus" AND (provider:"anthropic" OR provider:"openai") AND (api_key_id="k")`
	if got := s.filter(); got != want {
		t.Fatalf("filter =\n%s\nwant\n%s", got, want)
	}
	if got := (sessionSelector{}).filter(); got != "" {
		t.Fatalf("empty selector rendered %q", got)
	}
	if got := (sessionSelector{extra: " x=\"y\" "}).filter(); got != `x="y"` {
		t.Fatalf("bare extra = %q", got)
	}
}

func TestPromMatcher(t *testing.T) {
	if got := promMatcher("model", []string{"a.b"}); got != `model="a.b"` {
		t.Errorf("single = %s", got)
	}
	if got := promMatcher("model", []string{"a.b", "c|d"}); got != `model=~"a\\.b|c\\|d"` {
		t.Errorf("several = %s", got)
	}
	if got := promMatcher("model", nil); got != "" {
		t.Errorf("none = %q", got)
	}
}
