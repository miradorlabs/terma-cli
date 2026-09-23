package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"
)

// Paths of the AI read surface. Session-scoped reads identify the session with two
// query parameters rather than a path segment, because a session id is an opaque
// token the gateway mints and may hold anything.
const (
	aiSessionsPath             = "/v1/ai/sessions"
	aiSessionsStreamPath       = "/v1/ai/sessions/stream"
	aiSessionSummaryStreamPath = "/v1/ai/sessions/summary/stream"
	aiSessionEventsPath        = "/v1/ai/sessions/events"
	aiPrincipalsPath           = "/v1/ai/principals"
	aiGitPath                  = "/v1/ai/git-activities"
	aiGitStreamPath            = "/v1/ai/git-activities/stream"
)

// AISessionQuery selects and ranks sessions. Filter is an AIP-160 expression over
// source_system, user_id, api_key_id, model and provider. ActiveAfter keeps the
// sessions whose last activity is at or after it and accepts what the gateway accepts
// (RFC 3339 or a relative age); the gateway has no upper bound to match it. Sort is one
// of AISessionSorts and Page is 1-indexed. A zero value leaves the gateway's default.
type AISessionQuery struct {
	Filter      string
	ActiveAfter string
	Sort        string
	Page        int
	PerPage     int
}

func (q AISessionQuery) values() url.Values {
	v := url.Values{}
	setIfNotEmpty(v, "filter", q.Filter)
	setIfNotEmpty(v, "active_after", q.ActiveAfter)
	setIfNotEmpty(v, "sort", q.Sort)
	if q.Page > 0 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	if q.PerPage > 0 {
		v.Set("per_page", strconv.Itoa(q.PerPage))
	}
	return v
}

func setIfNotEmpty(v url.Values, key, value string) {
	if value != "" {
		v.Set(key, value)
	}
}

// sessionIdentity is the two-part key every session-scoped read takes.
func sessionIdentity(sessionID, sourceSystem string) url.Values {
	return url.Values{"session_id": {sessionID}, "source_system": {sourceSystem}}
}

// ListAISessions returns one page, ranked by q.Sort — most recently active first
// unless it says otherwise.
func (c *Client) ListAISessions(ctx context.Context, q AISessionQuery) (AISessionsPage, error) {
	var page AISessionsPage
	err := c.Get(ctx, aiSessionsPath, q.values(), &page)
	return page, err
}

// ForEachAISession walks the pages of a query from q.Page on, calling fn for every
// session until fn returns false or the last page is reached. The traversal is weakly
// consistent — a session that becomes active mid-walk moves up the ranking and pushes
// another onto the next page — so a session seen twice is delivered once. total_pages
// may grow while the walk runs, but a page short of the last has to bring a session
// not seen before: one that does not (empty, or all repeats), or one that answers for
// a page other than the one asked for, ends the walk as an error rather than a loop.
func (c *Client) ForEachAISession(ctx context.Context, q AISessionQuery, fn func(AISession) bool) error {
	if q.Page < 1 {
		q.Page = 1
	}
	seen := map[string]bool{}
	for {
		page, err := c.ListAISessions(ctx, q)
		if err != nil {
			return err
		}
		fresh := 0
		for _, s := range page.Sessions {
			key := s.SourceSystem + "\x00" + s.SessionID
			if seen[key] {
				continue
			}
			seen[key] = true
			fresh++
			if !fn(s) {
				return nil
			}
		}
		if q.Page >= page.Pagination.TotalPages {
			return nil
		}
		if got := page.Pagination.Page; got != q.Page {
			return fmt.Errorf("session listing answered with page %d when asked for page %d; results are incomplete", got, q.Page)
		}
		if fresh == 0 {
			return fmt.Errorf("session listing page %d of %d held no new session; results are incomplete", q.Page, page.Pagination.TotalPages)
		}
		q.Page++
	}
}

// GetAISession reads one session's roll-up. The gateway serves it only as a live
// feed — `summary` frames carrying {"summary": …}, the whole roll-up re-sent each
// tick — so this opens the feed, takes the first frame and hangs up. A feed has no
// request timeout of its own, so wait bounds the whole read: a gateway that accepts the
// connection and never sends a summary is an error, not a hang. A missing session is
// a 404 (IsNotFound).
func (c *Client) GetAISession(ctx context.Context, sessionID, sourceSystem string, wait time.Duration) (AISession, error) {
	bounded, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	// Only this call's own deadline means "no summary came"; a cancellation from the
	// caller stays the caller's.
	explain := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case bounded.Err() != nil:
			return fmt.Errorf("the gateway sent no summary of the session within %s", wait)
		case errors.Is(err, io.EOF):
			return fmt.Errorf("the gateway closed the session summary feed before sending a summary")
		}
		return err
	}

	stream, err := c.Stream(bounded, aiSessionSummaryStreamPath, sessionIdentity(sessionID, sourceSystem), "")
	if err != nil {
		return AISession{}, explain(err)
	}
	defer func() { _ = stream.Close() }()
	for {
		f, err := stream.Next()
		if err != nil {
			return AISession{}, explain(err)
		}
		switch f.Name {
		case "error":
			return AISession{}, AIStreamError(f)
		case "summary":
			var frame struct {
				Summary AISession `json:"summary"`
			}
			if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
				return AISession{}, fmt.Errorf("decode session summary: %w", err)
			}
			return frame.Summary, nil
		}
	}
}

// AISessionEventQuery pages and bounds one session's events. Cursor resumes after
// the event a previous page's NextCursor names; StartTime and EndTime are a half-open
// window over event time, and a zero time leaves that side open.
type AISessionEventQuery struct {
	Cursor    string
	StartTime time.Time
	EndTime   time.Time
	PerPage   int
}

func (q AISessionEventQuery) values(sessionID, sourceSystem string) url.Values {
	v := sessionIdentity(sessionID, sourceSystem)
	setIfNotEmpty(v, "cursor", q.Cursor)
	if !q.StartTime.IsZero() {
		v.Set("start_time", q.StartTime.UTC().Format(time.RFC3339Nano))
	}
	if !q.EndTime.IsZero() {
		v.Set("end_time", q.EndTime.UTC().Format(time.RFC3339Nano))
	}
	if q.PerPage > 0 {
		v.Set("per_page", strconv.Itoa(q.PerPage))
	}
	return v
}

// ListAISessionEvents reads one keyset page of a session's events, oldest first.
func (c *Client) ListAISessionEvents(ctx context.Context, sessionID, sourceSystem string, q AISessionEventQuery) (AISessionEventsResponse, error) {
	var out AISessionEventsResponse
	err := c.Get(ctx, aiSessionEventsPath, q.values(sessionID, sourceSystem), &out)
	return out, err
}

// AllAISessionEvents follows next_cursor from q.Cursor to the end and returns the
// session's history inside the window as one response, in conversational order. A
// session is bounded, so reading all of it is the normal case and the pages are as
// large as the gateway allows. An event that turns up again under its logical id — a
// correction that landed mid-walk — keeps its place and the higher version wins. A
// cursor that repeats ends the walk as an error rather than a loop.
func (c *Client) AllAISessionEvents(ctx context.Context, sessionID, sourceSystem string, q AISessionEventQuery) (AISessionEventsResponse, error) {
	if q.PerPage == 0 {
		q.PerPage = 1000
	}
	var out AISessionEventsResponse
	seenCursors := map[string]bool{}
	placed := map[string]int{}
	for {
		page, err := c.ListAISessionEvents(ctx, sessionID, sourceSystem, q)
		if err != nil {
			return AISessionEventsResponse{}, err
		}
		out.SessionID, out.SourceSystem = page.SessionID, page.SourceSystem
		for _, e := range page.Events {
			if at, again := placed[e.LogicalEventID]; again {
				if e.Version > out.Events[at].Version {
					out.Events[at] = e
				}
				continue
			}
			// An event without a logical id cannot be recognised again, so it is
			// never placed and always kept.
			if e.LogicalEventID != "" {
				placed[e.LogicalEventID] = len(out.Events)
			}
			out.Events = append(out.Events, e)
		}
		if page.NextCursor == "" {
			return out, nil
		}
		if seenCursors[page.NextCursor] {
			return AISessionEventsResponse{}, fmt.Errorf("session event listing repeated a cursor; results are incomplete")
		}
		seenCursors[page.NextCursor] = true
		q.Cursor = page.NextCursor
	}
}

// ListAIGitActivities reads a session's git and GitHub actions in event-time order.
// The gateway bounds it; there is no pagination.
func (c *Client) ListAIGitActivities(ctx context.Context, sessionID, sourceSystem string) (AIGitActivitiesResponse, error) {
	var out AIGitActivitiesResponse
	err := c.Get(ctx, aiGitPath, sessionIdentity(sessionID, sourceSystem), &out)
	return out, err
}

// StreamAISessions opens the live session catalog for the same slice as
// ListAISessions. The feed is the first page only, so q.Page does not travel. Frames:
// `snapshot` {"sessions": […], "pagination": …} — the whole page, sent on connect and
// re-sent on a fixed interval whether or not it changed, to replace rather than
// merge — and `heartbeat`. The gateway rotates the connection after an hour with a
// clean EOF.
func (c *Client) StreamAISessions(ctx context.Context, q AISessionQuery) (*Stream, error) {
	q.Page = 0
	return c.Stream(ctx, aiSessionsStreamPath, q.values(), "")
}

// StreamAIGitActivities opens a session's live git feed. Frames: `upsert`
// {"activity": …} keyed by activity_id, `snapshot_completed` {}, and `heartbeat`.
func (c *Client) StreamAIGitActivities(ctx context.Context, sessionID, sourceSystem string) (*Stream, error) {
	return c.Stream(ctx, aiGitStreamPath, sessionIdentity(sessionID, sourceSystem), "")
}

// AIStreamError turns the gateway's `error` frame into an error a person can act on.
func AIStreamError(f *Event) error {
	var detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(f.Data), &detail) != nil || detail.Message == "" {
		return fmt.Errorf("the stream reported an error and closed")
	}
	return &APIError{Code: detail.Code, Message: detail.Message}
}

// AIPrincipalQuery selects principals. Filter is an AIP-160 expression over kind
// ("user" or "api_key") and source_system.
type AIPrincipalQuery struct {
	Filter    string
	PageSize  int
	PageToken string
}

func (q AIPrincipalQuery) values() url.Values {
	v := url.Values{}
	setIfNotEmpty(v, "filter", q.Filter)
	if q.PageSize > 0 {
		v.Set("page_size", strconv.Itoa(q.PageSize))
	}
	setIfNotEmpty(v, "page_token", q.PageToken)
	return v
}

// ListAIPrincipals returns one page of the id→name catalog.
func (c *Client) ListAIPrincipals(ctx context.Context, q AIPrincipalQuery) (AIPrincipalsPage, error) {
	var page AIPrincipalsPage
	err := c.Get(ctx, aiPrincipalsPath, q.values(), &page)
	return page, err
}

// AllAIPrincipals follows every page of the catalog. Principals are bounded per
// tenant (people and keys, not sessions), so reading them all is the normal case.
func (c *Client) AllAIPrincipals(ctx context.Context, filter string) ([]AIPrincipal, error) {
	q := AIPrincipalQuery{Filter: filter, PageSize: 1000}
	var out []AIPrincipal
	seen := map[string]bool{}
	for {
		page, err := c.ListAIPrincipals(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Principals...)
		if page.NextPageToken == "" {
			return out, nil
		}
		if seen[page.NextPageToken] {
			return nil, fmt.Errorf("principal listing repeated a page token; results are incomplete")
		}
		seen[page.NextPageToken] = true
		q.PageToken = page.NextPageToken
	}
}
