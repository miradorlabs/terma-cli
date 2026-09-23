package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// retiredSessionParams are the names the gateway stopped reading. It ignores a
// parameter it does not know, so sending one is a filter that silently does nothing.
var retiredSessionParams = []string{"routing_key", "page_token", "page_size", "started_after", "started_before"}

func assertQuery(t *testing.T, r *http.Request, want url.Values) {
	t.Helper()
	got := r.URL.Query()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s query = %v, want %v", r.URL.Path, got, want)
	}
	for _, name := range retiredSessionParams {
		if got.Has(name) {
			t.Errorf("%s still sends the retired parameter %q", r.URL.Path, name)
		}
	}
}

// One page is one request, and every field of the query travels under the name the
// gateway reads today.
func TestListAISessions_SendsTheQueryAndReadsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		assertQuery(t, r, url.Values{
			"filter":       {`user_id="u1"`},
			"active_after": {"2026-09-01T00:00:00Z"},
			"sort":         {"cost"},
			"page":         {"3"},
			"per_page":     {"25"},
		})
		fmt.Fprint(w, `{"project_id":"p","sessions":[{"session_id":"s1","source_system":"codex","providers":["openai"],"git_orgs":["github.com/acme"],"repositories":["github.com/acme/app"]}],"pagination":{"page":3,"per_page":25,"total":51,"total_pages":3}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	page, err := c.ListAISessions(context.Background(), AISessionQuery{
		Filter: `user_id="u1"`, ActiveAfter: "2026-09-01T00:00:00Z", Sort: AISortCost, Page: 3, PerPage: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Pagination != (AIPagination{Page: 3, PerPage: 25, Total: 51, TotalPages: 3}) || page.ProjectID != "p" {
		t.Fatalf("page = %+v", page)
	}
	if s := page.Sessions[0]; s.SessionID != "s1" || len(s.Providers) != 1 || len(s.GitOrgs) != 1 || len(s.Repositories) != 1 {
		t.Fatalf("session = %+v", s)
	}
}

// An empty query sends nothing: the gateway's defaults are the gateway's to choose.
func TestListAISessions_EmptyQuerySendsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertQuery(t, r, url.Values{})
		fmt.Fprint(w, `{"sessions":[],"pagination":{"page":1,"per_page":100,"total":0,"total_pages":0},"project_id":"p"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	if _, err := c.ListAISessions(context.Background(), AISessionQuery{}); err != nil {
		t.Fatal(err)
	}
}

// The session walk has to reach every page exactly once, send the project header the
// gateway insists on, and hand back the integers it was given — a token count pushed
// through a float64 would come back wrong.
func TestForEachAISession_WalksPagesOnceAndKeepsPrecision(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-Mirador-Project") != "project-123" {
			t.Errorf("project header = %q", r.Header.Get("X-Mirador-Project"))
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		assertQuery(t, r, url.Values{"filter": {`user_id="u1"`}, "per_page": {"1"}, "page": {page}})
		switch page {
		case "1":
			fmt.Fprint(w, `{"project_id":"project-123","sessions":[{"session_id":"a","source_system":"codex","usage":{"input_tokens":9007199254740993,"provider_cost_usd":2.5}}],"pagination":{"page":1,"per_page":1,"total":2,"total_pages":2}}`)
		case "2":
			// The first session repeats (the traversal is weakly consistent), and the
			// list grew by a page while the walk ran. A second source system sharing
			// the session id is a different session.
			fmt.Fprint(w, `{"sessions":[{"session_id":"a","source_system":"codex"},{"session_id":"a","source_system":"claude-code"}],"pagination":{"page":2,"per_page":1,"total":3,"total_pages":3}}`)
		case "3":
			fmt.Fprint(w, `{"sessions":[{"session_id":"b","source_system":"codex"}],"pagination":{"page":3,"per_page":1,"total":3,"total_pages":3}}`)
		default:
			t.Errorf("unexpected page %q", page)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "project-123")
	var seen []AISession
	err := c.ForEachAISession(context.Background(), AISessionQuery{Filter: `user_id="u1"`, PerPage: 1}, func(s AISession) bool {
		seen = append(seen, s)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pages, ",") != "1,2,3" || len(seen) != 3 {
		t.Fatalf("pages=%v sessions=%d, want pages 1,2,3 and three sessions", pages, len(seen))
	}
	if u := seen[0].Usage; u.InputTokens != 9007199254740993 || u.CostUSD() != 2.5 {
		t.Fatalf("usage = %+v", u)
	}
}

// A walk can start on a later page, and then never asks for an earlier one.
func TestForEachAISession_StartsAtTheRequestedPage(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		fmt.Fprintf(w, `{"sessions":[{"session_id":"s%s","source_system":"codex"}],"pagination":{"page":%s,"per_page":1,"total":3,"total_pages":3}}`, page, page)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	n := 0
	err := c.ForEachAISession(context.Background(), AISessionQuery{Page: 2}, func(AISession) bool {
		n++
		return true
	})
	if err != nil || n != 2 || strings.Join(pages, ",") != "2,3" {
		t.Fatalf("err=%v sessions=%d pages=%v, want pages 2,3", err, n, pages)
	}
}

func TestForEachAISession_StopsWhenAsked(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, `{"sessions":[{"session_id":"a","source_system":"codex"},{"session_id":"b","source_system":"codex"}],"pagination":{"page":1,"per_page":2,"total":9,"total_pages":5}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	n := 0
	err := c.ForEachAISession(context.Background(), AISessionQuery{}, func(AISession) bool {
		n++
		return false
	})
	if err != nil || n != 1 || calls != 1 {
		t.Fatalf("err=%v visited=%d calls=%d, want a single visit and no second page", err, n, calls)
	}
}

// A gateway that misreports must end the walk, not spin it. Each of these claims one
// more page than it has just served, forever.
func TestForEachAISession_EndsOnAGatewayThatMisreports(t *testing.T) {
	cases := []struct {
		name      string
		respond   func(page int) string
		wantCalls int
	}{
		{
			// An empty page before the end.
			name: "empty pages",
			respond: func(page int) string {
				return fmt.Sprintf(`{"sessions":[],"pagination":{"page":%d,"per_page":1,"total":9,"total_pages":%d}}`, page, page+1)
			},
			wantCalls: 1,
		},
		{
			// total_pages grows with every request while the rows stay the same.
			name: "the same rows on every page",
			respond: func(page int) string {
				return fmt.Sprintf(`{"sessions":[{"session_id":"a","source_system":"codex"}],"pagination":{"page":%d,"per_page":1,"total":9,"total_pages":%d}}`, page, page+1)
			},
			wantCalls: 2,
		},
		{
			// The page parameter is ignored: page 1 comes back whatever was asked.
			name: "the wrong page",
			respond: func(page int) string {
				return fmt.Sprintf(`{"sessions":[{"session_id":"s%d","source_system":"codex"}],"pagination":{"page":1,"per_page":1,"total":9,"total_pages":9}}`, page)
			},
			wantCalls: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > 20 {
					t.Error("the walk is looping")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				fmt.Fprint(w, tc.respond(page))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL, liveCredential(), "p")
			err := c.ForEachAISession(context.Background(), AISessionQuery{}, func(AISession) bool { return true })
			if err == nil || !strings.Contains(err.Error(), "incomplete") {
				t.Fatalf("err = %v, want the walk to stop rather than loop", err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// Session-scoped reads identify the session with two query parameters. Losing the
// source system would make the gateway answer 400, and the old name for the id does
// the same: "session_id is required".
func TestSessionReadsCarryBothIdentityHalves(t *testing.T) {
	identity := url.Values{"session_id": {"sid/with slash"}, "source_system": {"claude-code"}}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/ai/sessions/summary/stream":
			assertQuery(t, r, identity)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: summary\ndata: {\"summary\":{\"session_id\":\"sid/with slash\",\"source_system\":\"claude-code\",\"turns\":3}}\n\n")
		case "/v1/ai/sessions/events":
			want := url.Values{"per_page": {"1000"}}
			maps.Copy(want, identity)
			assertQuery(t, r, want)
			fmt.Fprint(w, `{"session_id":"sid/with slash","source_system":"claude-code","events":[{"kind":"model_call","event_time":"2026-09-09T10:00:00Z","usage":{"output_tokens":5}}]}`)
		case "/v1/ai/git-activities":
			assertQuery(t, r, identity)
			fmt.Fprint(w, `{"session_id":"sid/with slash","source_system":"claude-code","activities":[{"activity_id":"x","action":"commit","event_time":"2026-09-09T10:00:00Z"}]}`)
		case "/v1/ai/git-activities/stream":
			assertQuery(t, r, identity)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: snapshot_completed\ndata: {}\n\n")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	ctx := context.Background()
	s, err := c.GetAISession(ctx, "sid/with slash", "claude-code", 5*time.Second)
	if err != nil || s.Turns != 3 || s.SessionID != "sid/with slash" {
		t.Fatalf("get: %v %+v", err, s)
	}
	ev, err := c.AllAISessionEvents(ctx, "sid/with slash", "claude-code", AISessionEventQuery{})
	if err != nil || ev.SessionID != "sid/with slash" || len(ev.Events) != 1 || ev.Events[0].Kind != AIEventModelCall || ev.Events[0].Usage.OutputTokens != 5 {
		t.Fatalf("events: %v %+v", err, ev)
	}
	git, err := c.ListAIGitActivities(ctx, "sid/with slash", "claude-code")
	if err != nil || git.SessionID != "sid/with slash" || len(git.Activities) != 1 || git.Activities[0].Action != "commit" {
		t.Fatalf("git: %v %+v", err, git)
	}
	stream, err := c.StreamAIGitActivities(ctx, "sid/with slash", "claude-code")
	if err != nil {
		t.Fatalf("git stream: %v", err)
	}
	_ = stream.Close()
	if len(paths) != 4 {
		t.Fatalf("paths = %v", paths)
	}
}

// heldFeed answers 200 with the frames given, then keeps the connection open the way
// a live feed does. released closes once the client has hung up.
func heldFeed(t *testing.T, frames string) (srv *httptest.Server, released chan struct{}) {
	t.Helper()
	released = make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, frames)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(released)
	}))
	t.Cleanup(srv.Close)
	return srv, released
}

// The roll-up of one session is served as a feed that never ends. The read takes the
// first summary — past whatever else the feed carries — and hangs up.
func TestGetAISession_TakesTheFirstSummaryAndHangsUp(t *testing.T) {
	srv, released := heldFeed(t, "retry: 3000\n\n"+
		"event: heartbeat\ndata: {}\n\n"+
		"event: summary\ndata: {\"summary\":{\"session_id\":\"s1\",\"source_system\":\"codex\",\"turns\":7,\"usage\":{\"input_tokens\":10,\"provider_cost_usd\":0.25}}}\n\n"+
		"event: summary\ndata: {\"summary\":{\"session_id\":\"s1\",\"source_system\":\"codex\",\"turns\":8}}\n\n")

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	s, err := c.GetAISession(context.Background(), "s1", "codex", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.Turns != 7 || s.Usage.CostUSD() != 0.25 {
		t.Fatalf("session = %+v, want the first summary frame", s)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the feed was left open after the summary was read")
	}
}

func TestGetAISession_MissingSessionIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"NOT_FOUND","message":"session not found"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	_, err := c.GetAISession(context.Background(), "nope", "codex", 5*time.Second)
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want a 404 the caller can recognise", err)
	}
}

// A feed that connects and then says nothing would hold the command forever: the
// stream client has no timeout of its own. The wait turns that into an error.
func TestGetAISession_SilentFeedEndsAtTheDeadline(t *testing.T) {
	heartbeats, _ := heldFeed(t, "event: heartbeat\ndata: {}\n\n")
	// The other way to say nothing: accept the request and never answer it.
	mute := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer mute.Close()

	for name, srv := range map[string]*httptest.Server{"heartbeats only": heartbeats, "no response": mute} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, srv.URL, liveCredential(), "p")
			started := time.Now()
			_, err := c.GetAISession(context.Background(), "s1", "codex", 100*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "no summary") || !strings.Contains(err.Error(), "100ms") {
				t.Fatalf("err = %v, want the deadline named", err)
			}
			if took := time.Since(started); took > 5*time.Second {
				t.Fatalf("took %s to give up on a 100ms wait", took)
			}
		})
	}
}

// The caller's own cancellation is not "the gateway sent nothing".
func TestGetAISession_CallerCancellationStaysTheCallers(t *testing.T) {
	srv, _ := heldFeed(t, "")

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := c.GetAISession(ctx, "s1", "codex", 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestGetAISession_SurfacesFeedFailures(t *testing.T) {
	cases := []struct{ name, frames, want string }{
		{"an error frame", "event: error\ndata: {\"code\":\"UNAVAILABLE\",\"message\":\"backend went away\"}\n\n", "backend went away"},
		{"a feed that closes first", "event: heartbeat\ndata: {}\n\n", "before sending a summary"},
		{"a summary that is not JSON", "event: summary\ndata: nope\n\n", "decode session summary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseServer(t, tc.frames, nil)
			defer srv.Close()
			c := newTestClient(t, srv.URL, liveCredential(), "p")
			_, err := c.GetAISession(context.Background(), "s1", "codex", 5*time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// The event history is keyset-paged now. The walk carries the window and the identity
// onto every page, follows next_cursor to the end, and folds a correction that
// arrives under a logical id already seen into the place the event first had.
func TestAllAISessionEvents_FollowsCursorsAndFoldsCorrections(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions/events" {
			t.Errorf("path = %s", r.URL.Path)
		}
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		want := url.Values{
			"session_id":    {"s1"},
			"source_system": {"codex"},
			"start_time":    {"2026-09-09T09:00:00Z"},
			"end_time":      {"2026-09-09T10:00:00.5Z"},
			"per_page":      {"1000"},
		}
		if cursor != "" {
			want.Set("cursor", cursor)
		}
		assertQuery(t, r, want)
		switch cursor {
		case "":
			fmt.Fprint(w, `{"session_id":"s1","source_system":"codex","next_cursor":"c1","events":[
				{"kind":"user_message","event_time":"2026-09-09T09:00:01Z","logical_event_id":"e1","version":1,"cursor":"k1"},
				{"kind":"model_call","event_time":"2026-09-09T09:00:02Z","logical_event_id":"e2","version":1,"status":"pending"}]}`)
		case "c1":
			fmt.Fprint(w, `{"session_id":"s1","source_system":"codex","next_cursor":"c2","events":[
				{"kind":"model_call","event_time":"2026-09-09T09:00:02Z","logical_event_id":"e2","version":2,"status":"ok"},
				{"kind":"user_message","event_time":"2026-09-09T09:00:01Z","logical_event_id":"e1","version":1},
				{"kind":"tool_call","event_time":"2026-09-09T09:00:03Z"}]}`)
		case "c2":
			fmt.Fprint(w, `{"session_id":"s1","source_system":"codex","events":[{"kind":"tool_call","event_time":"2026-09-09T09:00:04Z"}]}`)
		default:
			t.Errorf("unexpected cursor %q", cursor)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	out, err := c.AllAISessionEvents(context.Background(), "s1", "codex", AISessionEventQuery{
		StartTime: time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 9, 9, 10, 0, 0, 500_000_000, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cursors, ",") != ",c1,c2" {
		t.Fatalf("cursors = %q", cursors)
	}
	if out.SessionID != "s1" || out.SourceSystem != "codex" || out.NextCursor != "" {
		t.Fatalf("response = %+v", out)
	}
	var got []string
	for _, e := range out.Events {
		got = append(got, e.Kind+"/"+e.LogicalEventID+"/"+e.Status)
	}
	// e2's correction replaced it in place, e1's repeat was dropped, and the two
	// events with no logical id were both kept.
	if want := "user_message/e1/,model_call/e2/ok,tool_call//,tool_call//"; strings.Join(got, ",") != want {
		t.Fatalf("events = %v, want %s", got, want)
	}
	if out.Events[0].Cursor != "k1" || out.Events[1].Version != 2 {
		t.Fatalf("events = %+v", out.Events)
	}
}

func TestAllAISessionEvents_RejectsRepeatedCursor(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > 20 {
			t.Error("the walk is looping")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"session_id":"s1","source_system":"codex","events":[],"next_cursor":"same"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	_, err := c.AllAISessionEvents(context.Background(), "s1", "codex", AISessionEventQuery{})
	if err == nil || !strings.Contains(err.Error(), "incomplete") || calls != 2 {
		t.Fatalf("err = %v after %d calls, want the walk to stop rather than loop", err, calls)
	}
}

// The live catalog takes the list's slice but only ever its first page.
func TestStreamAISessions_SendsTheSliceWithoutAPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions/stream" {
			t.Errorf("path = %s", r.URL.Path)
		}
		assertQuery(t, r, url.Values{
			"filter":       {`source_system="codex"`},
			"active_after": {"2026-09-01T00:00:00Z"},
			"sort":         {"tokens"},
			"per_page":     {"10"},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: snapshot\ndata: {\"sessions\":[],\"pagination\":{\"page\":1,\"per_page\":10,\"total\":0,\"total_pages\":0}}\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	stream, err := c.StreamAISessions(context.Background(), AISessionQuery{
		Filter: `source_system="codex"`, ActiveAfter: "2026-09-01T00:00:00Z", Sort: AISortTokens, Page: 4, PerPage: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if f, err := stream.Next(); err != nil || f.Name != "snapshot" {
		t.Fatalf("frame = %+v, %v", f, err)
	}
}

func TestAllAIPrincipals_FollowsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/principals" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("filter"); got != `kind="user"` {
			t.Errorf("filter = %q", got)
		}
		if r.URL.Query().Get("page_token") == "" {
			fmt.Fprint(w, `{"principals":[{"kind":"user","id":"u1","name":"a@x.io","source_system":"claude-code"}],"next_page_token":"n"}`)
			return
		}
		fmt.Fprint(w, `{"principals":[{"kind":"user","id":"u2","name":"b@x.io","source_system":"codex","alias":"Bee"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	ps, err := c.AllAIPrincipals(context.Background(), `kind="user"`)
	if err != nil || len(ps) != 2 {
		t.Fatalf("err=%v principals=%+v", err, ps)
	}
	if ps[1].DisplayName() != "Bee" || ps[0].DisplayName() != "a@x.io" {
		t.Fatalf("display names: %q %q", ps[0].DisplayName(), ps[1].DisplayName())
	}
}

// The metric value is Prometheus's [time, "string"] pair; the string form is how
// precision survives the wire, and the parse has to accept what Prometheus emits.
func TestQueryMetric_DecodesInstantVector(t *testing.T) {
	var gotQuery, gotTime string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics/query" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotQuery = r.URL.Query().Get("query")
		gotTime = r.URL.Query().Get("time")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"user_id":"u1","source_system":"claude-code"},"value":[1757412000.123,"1234.5"]},{"metric":{},"value":[1757412000,"NaN"]}]}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	at := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	res, err := c.QueryMetric(context.Background(), `sum(increase({__name__="terma.ai.cost.usd.total"}[86400s]))`, at)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery == "" || gotTime != "2026-09-09T10:00:00Z" {
		t.Fatalf("query=%q time=%q", gotQuery, gotTime)
	}
	if len(res.Data.Result) != 2 || res.Data.Result[0].Metric["user_id"] != "u1" {
		t.Fatalf("result = %+v", res.Data.Result)
	}
	if v, err := res.Data.Result[0].Float(); err != nil || v != 1234.5 {
		t.Fatalf("value = %v %v", v, err)
	}
	if _, err := res.Data.Result[1].Float(); err != nil {
		t.Fatalf("NaN must parse: %v", err)
	}
}

func TestQueryMetric_SurfacesPrometheusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	if _, err := c.QueryMetric(context.Background(), "x", time.Time{}); err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("err = %v", err)
	}
}

func TestAIStreamError(t *testing.T) {
	err := AIStreamError(&Event{Name: "error", Data: `{"code":"UNAVAILABLE","message":"backend went away"}`})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Message != "backend went away" || apiErr.Code != "UNAVAILABLE" {
		t.Fatalf("err = %#v", err)
	}
	if err := AIStreamError(&Event{Name: "error", Data: `garbage`}); err == nil {
		t.Fatal("a malformed error frame must still be an error")
	}
}
