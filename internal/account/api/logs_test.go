package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// commitLogsBody is a verbatim /v1/logs response captured from the dev backend for a
// real terma.commit record. It is the contract this decoder is written against: the
// records live under "logs", every attribute value is a string, and the event's own
// attributes are nested under "attributes" while the exporter's are under
// "resource_attributes".
const commitLogsBody = `{
  "logs": [
    {
      "attributes": {
        "author_email": "dawsonwalker91@gmail.com",
        "branch": "feat/org-switching-and-use",
        "file_count": "32",
        "file_stats_reported": "32",
        "file_stats_truncated": "false",
        "lines_added": "2609",
        "lines_deleted": "246",
        "project_id": "3f594407-aec5-4ab7-9d7b-9e4ebe5822f0",
        "repo_url": "https://github.com/miradorlabs/terma-cli",
        "session.id": "583683ec-8e0c-4fd5-b1a6-c97b74be7f5e",
        "session_count": "1",
        "sessions": "583683ec-8e0c-4fd5-b1a6-c97b74be7f5e",
        "sha": "bbdddb691106f0f158ed3333665d2389621dce78",
        "terma.repo": "terma-cli",
        "tool": "claude-code"
      },
      "body": "terma.commit",
      "event_name": "terma.commit",
      "observed_time": "2026-09-14T18:44:24.980723Z",
      "resource_attributes": {
        "mirador.project.id": "3f594407-aec5-4ab7-9d7b-9e4ebe5822f0",
        "service.version": "17a623b-dirty"
      },
      "scope_name": "terma-cli",
      "service_name": "terma-cli",
      "severity_number": 9,
      "severity_text": "INFO",
      "time": "2026-09-14T18:44:24.980723Z"
    }
  ],
  "project_id": "3f594407-aec5-4ab7-9d7b-9e4ebe5822f0",
  "range_end": "2026-09-16T00:00:00Z",
  "range_start": "2026-08-20T00:00:00Z"
}`

func TestCommitLog_DecodesRealRecordAndBuildsQuery(t *testing.T) {
	const sha = "bbdddb691106f0f158ed3333665d2389621dce78"
	var gotQuery = make(map[string]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		for k := range r.URL.Query() {
			gotQuery[k] = r.URL.Query().Get(k)
		}
		fmt.Fprint(w, commitLogsBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "project-123")
	since := time.Date(2026, 9, 14, 17, 0, 0, 0, time.UTC)
	until := time.Date(2026, 9, 14, 20, 0, 0, 0, time.UTC)
	rec, err := c.CommitLog(context.Background(), sha, since, until)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("record = nil, want the terma.commit record")
	}

	// The filter names the event and the sha, and the window travels as since/until.
	wantFilter := `attribute.event.name="terma.commit" AND attribute.sha="` + sha + `"`
	if gotQuery["filter"] != wantFilter {
		t.Errorf("filter = %q\nwant     %q", gotQuery["filter"], wantFilter)
	}
	if gotQuery["since"] != "2026-09-14T17:00:00Z" || gotQuery["until"] != "2026-09-14T20:00:00Z" {
		t.Errorf("window = %q..%q", gotQuery["since"], gotQuery["until"])
	}
	if gotQuery["limit"] != "1" {
		t.Errorf("limit = %q", gotQuery["limit"])
	}

	// The string-valued, nested attributes decode through the typed accessors.
	if rec.EventName != "terma.commit" {
		t.Errorf("event_name = %q", rec.EventName)
	}
	if rec.Attr("session.id") != "583683ec-8e0c-4fd5-b1a6-c97b74be7f5e" || rec.Attr("sessions") != "583683ec-8e0c-4fd5-b1a6-c97b74be7f5e" {
		t.Errorf("session.id/sessions = %q / %q", rec.Attr("session.id"), rec.Attr("sessions"))
	}
	if rec.Attr("sha") != sha || rec.Attr("tool") != "claude-code" {
		t.Errorf("sha/tool = %q / %q", rec.Attr("sha"), rec.Attr("tool"))
	}
	if rec.Int("lines_added") != 2609 || rec.Int("lines_deleted") != 246 || rec.Int("file_count") != 32 {
		t.Errorf("lines = +%d/-%d over %d", rec.Int("lines_added"), rec.Int("lines_deleted"), rec.Int("file_count"))
	}
	if rec.Attr("branch") != "feat/org-switching-and-use" || rec.Attr("repo_url") != "https://github.com/miradorlabs/terma-cli" {
		t.Errorf("branch/repo = %q / %q", rec.Attr("branch"), rec.Attr("repo_url"))
	}
	if rec.ResourceAttr("mirador.project.id") != "3f594407-aec5-4ab7-9d7b-9e4ebe5822f0" {
		t.Errorf("resource project id = %q", rec.ResourceAttr("mirador.project.id"))
	}
	if rec.Time.IsZero() {
		t.Error("time did not decode")
	}
}

func TestCommitLog_MissIsNilNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The shape a miss actually returns: an empty logs array, no records key.
		fmt.Fprint(w, `{"logs":[],"project_id":"p","range_start":"2026-09-15T23:00:00Z","range_end":"2026-09-16T00:00:00Z"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, liveCredential(), "p")
	rec, err := c.CommitLog(context.Background(), "deadbeef", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("a miss must not be an error: %v", err)
	}
	if rec != nil {
		t.Fatalf("record = %+v, want nil on a miss", rec)
	}
}

func TestLogQuote(t *testing.T) {
	cases := map[string]string{
		"abc":       `"abc"`,
		`a"b`:       `"a\"b"`,
		`a\b`:       `"a\\b"`,
		`"; DROP;"`: `"\"; DROP;\""`,
	}
	for in, want := range cases {
		if got := logQuote(in); got != want {
			t.Errorf("logQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
