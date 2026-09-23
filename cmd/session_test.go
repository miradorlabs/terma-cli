package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These drive `terma session` against a fake gateway (runInsights) and hold it to the
// query parameters the gateway reads today. It ignores a name it does not know, so a
// stale one is not an error anyone sees — it is a flag that quietly does nothing.

func wantQuery(t *testing.T, r *http.Request, want url.Values) {
	t.Helper()
	if got := r.URL.Query(); !reflect.DeepEqual(got, want) {
		t.Errorf("%s query = %v, want %v", r.URL.Path, got, want)
	}
}

func sessionRow(id, lastActive string) string {
	if lastActive == "" {
		return fmt.Sprintf(`{"session_id":%q,"source_system":"codex"}`, id)
	}
	return fmt.Sprintf(`{"session_id":%q,"source_system":"codex","last_activity_at":%q}`, id, lastActive)
}

func sessionsPage(page, totalPages int, rows ...string) string {
	return fmt.Sprintf(`{"project_id":"p","sessions":[%s],"pagination":{"page":%d,"per_page":2,"total":%d,"total_pages":%d}}`,
		strings.Join(rows, ","), page, totalPages*2, totalPages)
}

func listedIDs(t *testing.T, stdout string) []string {
	t.Helper()
	var view sessionListView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	ids := make([]string, 0, len(view.Sessions))
	for _, s := range view.Sessions {
		ids = append(ids, s.SessionID)
	}
	return ids
}

func TestSessionList_SendsSortPageAndPageSize(t *testing.T) {
	handler := principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		wantQuery(t, r, url.Values{
			"filter":   {`provider:"anthropic"`},
			"sort":     {"cost"},
			"page":     {"2"},
			"per_page": {"2"},
		})
		fmt.Fprint(w, sessionsPage(2, 3, sessionRow("c", ""), sessionRow("d", "")))
	})
	args := []string{"session", "list", "--provider", "anthropic", "--sort", "cost", "--page", "2", "--page-size", "2"}

	stdout, _, err := runInsights(t, handler, args...)
	if err != nil {
		t.Fatal(err)
	}
	var view sessionListView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if view.Pagination == nil || view.Pagination.Page != 2 || view.Pagination.TotalPages != 3 || len(view.Sessions) != 2 {
		t.Fatalf("view = %+v", view)
	}
	if strings.Contains(stdout, "routing_key") || strings.Contains(stdout, "next_page_token") {
		t.Fatalf("stdout still carries the old contract:\n%s", stdout)
	}

	// A person reading the table is told how to get the next page.
	stdout, stderr, err := runInsights(t, handler, append(args, "-o", "table")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Page 2 of 3") || !strings.Contains(stderr, "--page 3") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stdout, "SESSION ID") || strings.Contains(stdout, "ROUTING KEY") {
		t.Fatalf("table = %s", stdout)
	}
}

func TestSessionList_RejectsBadFlagsBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an unknown ranking", []string{"--sort", "price"}, "recency, cost, tokens, turns, tools"},
		{"a page before the first", []string{"--page", "-1"}, "--page"},
		{"a page larger than the gateway serves", []string{"--page-size", "1001"}, "--page-size"},
		{"a tail of a later page", []string{"--follow", "--page", "2"}, "--follow"},
		{"a tail with an upper bound", []string{"--follow", "--until", "today"}, "--follow"},
		{"a server page of a client-side filter", []string{"--until", "today", "--page", "2"}, "--until"},
		{"the retired token flag", []string{"--page-token", "abc"}, "unknown flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runInsights(t, func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("no request expected; got %s", r.URL.Path)
			}, append([]string{"session", "list"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// The gateway bounds last activity from below only, so --until is the client's: it
// never travels, it is a half-open bound on the same field, and the walk keeps reading
// pages until a page of ours is full — then stops, without reading the rest.
func TestSessionList_UntilFiltersClientSideAcrossPages(t *testing.T) {
	pages := map[string]string{
		// Ranked by recency, so the rows --until rejects come first.
		"1": sessionsPage(1, 4, sessionRow("s1", "2026-09-10T00:00:00Z"), sessionRow("s2", "2026-09-09T00:00:00Z")),
		// A session that reports no last activity cannot be placed before anything.
		"2": sessionsPage(2, 4, sessionRow("s3", "2026-09-07T23:59:59Z"), sessionRow("s4", "")),
		// The bound is exclusive.
		"3": sessionsPage(3, 4, sessionRow("s5", "2026-09-08T00:00:00Z"), sessionRow("s6", "2026-09-06T00:00:00Z")),
		"4": sessionsPage(4, 4, sessionRow("s7", "2026-09-02T00:00:00Z")),
	}
	var asked []string
	handler := principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		asked = append(asked, page)
		wantQuery(t, r, url.Values{"active_after": {"2026-09-01T00:00:00Z"}, "per_page": {"2"}, "page": {page}})
		fmt.Fprint(w, pages[page])
	})
	args := []string{"session", "list", "--since", "2026-09-01T00:00:00Z", "--until", "2026-09-08T00:00:00Z", "--page-size", "2"}

	stdout, _, err := runInsights(t, handler, args...)
	if err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, stdout); strings.Join(got, ",") != "s3,s6" {
		t.Fatalf("sessions = %v, want s3,s6", got)
	}
	if strings.Join(asked, ",") != "1,2,3" {
		t.Fatalf("pages asked = %v, want the walk to stop once --page-size rows passed", asked)
	}
	if strings.Contains(stdout, `"pagination"`) {
		t.Fatalf("a filtered walk claimed a server page:\n%s", stdout)
	}

	asked = nil
	stdout, _, err = runInsights(t, handler, append(args, "--all")...)
	if err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, stdout); strings.Join(got, ",") != "s3,s6,s7" || strings.Join(asked, ",") != "1,2,3,4" {
		t.Fatalf("--all: sessions = %v over pages %v", got, asked)
	}

	// A table says when it stopped short, since nothing else would.
	_, stderr, err := runInsights(t, handler, append(args, "-o", "table")...)
	if err != nil || !strings.Contains(stderr, "Stopped at 2 sessions") {
		t.Fatalf("err = %v stderr = %q", err, stderr)
	}
}

// A relative --until is an age the gateway never sees; it is resolved to an instant
// here, against the clock of the machine asking.
func TestSessionList_UntilResolvesRelativeAges(t *testing.T) {
	now := time.Now().UTC()
	stdout, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		wantQuery(t, r, url.Values{"page": {"1"}})
		fmt.Fprint(w, sessionsPage(1, 1,
			sessionRow("recent", now.Add(-30*time.Minute).Format(time.RFC3339)),
			sessionRow("older", now.Add(-2*time.Hour).Format(time.RFC3339))))
	}), "session", "list", "--until", "1h")
	if err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, stdout); strings.Join(got, ",") != "older" {
		t.Fatalf("sessions = %v, want only the one last active more than an hour ago", got)
	}
}

// A walk that the gateway cannot finish honestly is an error, never a short list
// passed off as the whole one.
func TestSessionList_AllReportsAnIncompleteWalk(t *testing.T) {
	calls := 0
	_, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 20 {
			t.Error("the walk is looping")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, sessionsPage(calls, calls+1))
	}), "session", "list", "--all")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}

const summaryFrame = "event: summary\ndata: {\"summary\":{\"session_id\":\"sid1\",\"source_system\":\"claude-code\",\"user_id\":\"u-dana\",\"turns\":7,\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"provider_cost_usd\":0.25},\"providers\":[\"anthropic\"]}}\n\n"

// The gateway serves one session's roll-up only as a feed. `get` reads its first
// summary and returns; the feed staying open behind it is not the command's problem.
func TestSessionGet_ReadsTheSummaryFeed(t *testing.T) {
	stdout, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions/summary/stream" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		wantQuery(t, r, url.Values{"session_id": {"sid1"}, "source_system": {"claude-code"}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n"+summaryFrame)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), "session", "get", "sid1", "--source", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	var view sessionView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if view.SessionID != "sid1" || view.Turns != 7 || view.UserName != "Dana" || view.Usage.CostUSD() != 0.25 {
		t.Fatalf("view = %+v", view)
	}
	if strings.Contains(stdout, "routing_key") {
		t.Fatalf("stdout still carries a routing key:\n%s", stdout)
	}
}

// A feed that opens and never sends a summary has to end as an error a person can
// read. The wait is shortened here; runInsights' own deadline would fail this test
// with a context error if the command's did not fire first.
func TestSessionGet_SilentFeedIsAnErrorNotAHang(t *testing.T) {
	previous := sessionGetWait
	sessionGetWait = 100 * time.Millisecond
	t.Cleanup(func() { sessionGetWait = previous })

	_, _, err := runInsights(t, principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), "session", "get", "sid1", "--source", "claude-code")
	if err == nil || !strings.Contains(err.Error(), "no summary") {
		t.Fatalf("err = %v", err)
	}
}

// The history is keyset-paged. The command still prints all of it, sends the window
// to the gateway, and applies the same window itself in case the gateway did not.
func TestSessionEvents_WalksCursorsAndSendsTheWindow(t *testing.T) {
	var cursors []string
	stdout, _, err := runInsights(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions/events" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		want := url.Values{
			"session_id":    {"sid1"},
			"source_system": {"antigravity"},
			"start_time":    {"2026-09-09T09:00:00Z"},
			"end_time":      {"2026-09-09T10:00:00Z"},
			"per_page":      {"1000"},
		}
		if cursor != "" {
			want.Set("cursor", cursor)
		}
		wantQuery(t, r, want)
		if cursor == "" {
			// The first event is before the window: a gateway that ignored start_time.
			fmt.Fprint(w, `{"session_id":"sid1","source_system":"antigravity","next_cursor":"c1","events":[
				{"kind":"user_message","event_time":"2026-09-09T08:59:59Z","logical_event_id":"e0"},
				{"kind":"user_message","event_time":"2026-09-09T09:00:00Z","logical_event_id":"e1"}]}`)
			return
		}
		fmt.Fprint(w, `{"session_id":"sid1","source_system":"antigravity","events":[
			{"kind":"tool_call","event_time":"2026-09-09T09:30:00Z","logical_event_id":"e2","cursor":"k2"},
			{"kind":"tool_call","event_time":"2026-09-09T10:00:00Z","logical_event_id":"e3"}]}`)
	}, "session", "events", "sid1", "--source", "antigravity", "--since", "2026-09-09T09:00:00Z", "--until", "2026-09-09T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cursors, ",") != ",c1" {
		t.Fatalf("cursors = %q", cursors)
	}
	var resp struct {
		SessionID  string `json:"session_id"`
		NextCursor string `json:"next_cursor"`
		Events     []struct {
			LogicalEventID string `json:"logical_event_id"`
			Cursor         string `json:"cursor"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if resp.SessionID != "sid1" || resp.NextCursor != "" || len(resp.Events) != 2 ||
		resp.Events[0].LogicalEventID != "e1" || resp.Events[1].LogicalEventID != "e2" || resp.Events[1].Cursor != "k2" {
		t.Fatalf("events = %+v\n%s", resp, stdout)
	}
}

func TestSessionEvents_RepeatedCursorIsAnError(t *testing.T) {
	calls := 0
	_, _, err := runInsights(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > 20 {
			t.Error("the walk is looping")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"session_id":"sid1","source_system":"codex","events":[{"kind":"turn_start","event_time":"2026-09-09T09:00:00Z"}],"next_cursor":"again"}`)
	}, "session", "events", "sid1", "--source", "codex")
	if err == nil || !strings.Contains(err.Error(), "incomplete") || calls != 2 {
		t.Fatalf("err = %v after %d calls, want the walk to stop rather than loop", err, calls)
	}
}

func TestSessionGit_IdentifiesTheSessionByID(t *testing.T) {
	stdout, _, err := runInsights(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/git-activities" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		wantQuery(t, r, url.Values{"session_id": {"sid1"}, "source_system": {"claude-code"}})
		fmt.Fprint(w, `{"session_id":"sid1","source_system":"claude-code","activities":[{"activity_id":"a1","action":"commit","event_time":"2026-09-09T10:00:00Z","source_system":"claude-code"}]}`)
	}, "session", "git", "sid1", "--source", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"session_id": "sid1"`) || strings.Contains(stdout, "routing_key") {
		t.Fatalf("stdout = %s", stdout)
	}

	_, _, err = runInsights(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"NOT_FOUND","message":"session not found"}}`)
	}, "session", "git", "nope", "--source", "claude-code")
	if err == nil || !strings.Contains(err.Error(), "copy the session id and source") {
		t.Fatalf("err = %v", err)
	}
}

// The live catalog is not a change feed: the gateway re-sends the whole first page on
// a timer. Repeats are dropped, JSON passes the rest through, and a table prints only
// the sessions that are new or changed since they were last printed.
func TestSessionListFollow_ReadsSnapshotFrames(t *testing.T) {
	first := `{"sessions":[` + sessionRow("s1", "2026-09-09T10:00:00Z") + `,` + sessionRow("s2", "2026-09-09T09:00:00Z") + `],"pagination":{"page":1,"per_page":5,"total":2,"total_pages":1}}`
	second := `{"sessions":[` + sessionRow("s3", "2026-09-09T10:02:00Z") + `,` + sessionRow("s1", "2026-09-09T10:01:00Z") + `,` + sessionRow("s2", "2026-09-09T09:00:00Z") + `],"pagination":{"page":1,"per_page":5,"total":3,"total_pages":1}}`
	handler := principalsThen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions/stream" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		wantQuery(t, r, url.Values{"filter": {`source_system="codex"`}, "sort": {"tokens"}, "per_page": {"5"}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: snapshot\ndata: "+first+"\n\n"+
			"event: heartbeat\ndata: {}\n\n"+
			"event: snapshot\ndata: "+first+"\n\n"+
			"event: snapshot\ndata: "+second+"\n\n")
	})
	args := []string{"session", "list", "--follow", "--source", "codex", "--sort", "tokens", "--page-size", "5"}

	stdout, _, err := runInsights(t, handler, args...)
	if err == nil || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("the hourly rotation must surface as an error, got %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("frames:\n%s", stdout)
	}
	for i, line := range lines {
		var env struct {
			Event string `json:"event"`
			Data  struct {
				Sessions []json.RawMessage `json:"sessions"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil || env.Event != "snapshot" || len(env.Data.Sessions) != 2+i {
			t.Fatalf("frame %d = %s (%v)", i, line, err)
		}
	}

	stdout, stderr, _ := runInsights(t, handler, append(args, "-o", "table")...)
	var ids []string
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		fields := strings.Fields(line)
		ids = append(ids, fields[len(fields)-1])
	}
	// s2 did not change between the snapshots, so it is printed once.
	if strings.Join(ids, ",") != "s1,s2,s3,s1" {
		t.Fatalf("lines = %v\n%s", ids, stdout)
	}
	if strings.Count(stderr, "Snapshot complete") != 1 {
		t.Fatalf("stderr = %q", stderr)
	}
}
