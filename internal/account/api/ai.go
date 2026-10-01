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

// Paths of the AI read surface. A session id is opaque and may hold anything, so
// session-scoped reads pass it as a query parameter, never a path segment.
const (
	aiSessionsPath             = "/v1/ai/sessions"
	aiSessionsStreamPath       = "/v1/ai/sessions/stream"
	aiSessionSummaryStreamPath = "/v1/ai/sessions/summary/stream"
	aiSessionEventsPath        = "/v1/ai/sessions/events"
	aiPrincipalsPath           = "/v1/ai/principals"
	aiGitPath                  = "/v1/ai/git-activities"
	aiGitStreamPath            = "/v1/ai/git-activities/stream"
)

// AISessionQuery selects and ranks sessions: Filter is AIP-160, ActiveAfter has no upper
// bound to match it, Sort is one of AISessionSorts and Page is 1-indexed.
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

// ListAISessions returns one page, ranked by q.Sort.
func (c *Client) ListAISessions(ctx context.Context, q AISessionQuery) (AISessionsPage, error) {
	var page AISessionsPage
	err := c.Get(ctx, aiSessionsPath, q.values(), &page)
	return page, err
}

// ForEachAISession calls fn for every session from q.Page on until it returns false. The
// ranking moves mid-walk, so a repeat is delivered once and a page with nothing new ends
// the walk as an error rather than a loop.
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

// GetAISession reads the first frame of a session's summary feed, the only way the
// gateway serves a roll-up; wait bounds the read, since a feed has no timeout of its own.
func (c *Client) GetAISession(ctx context.Context, sessionID, sourceSystem string, wait time.Duration) (AISession, error) {
	bounded, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	// Only this call's own deadline means "no summary came".
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

// AISessionEventQuery pages one session's events; StartTime and EndTime are a half-open
// window, and a zero time leaves that side open.
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

// AllAISessionEvents reads a session's events from q.Cursor to the end as one response.
// An event seen again under its logical id keeps its place with the higher version, and
// a repeated cursor ends the walk as an error rather than a loop.
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
			// An event without a logical id cannot be recognised again, so it is always kept.
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

// ListAIGitActivities reads a session's git and GitHub actions in event-time order, unpaginated.
func (c *Client) ListAIGitActivities(ctx context.Context, sessionID, sourceSystem string) (AIGitActivitiesResponse, error) {
	var out AIGitActivitiesResponse
	err := c.Get(ctx, aiGitPath, sessionIdentity(sessionID, sourceSystem), &out)
	return out, err
}

// StreamAISessions opens the live first page of ListAISessions: `snapshot` frames that
// replace the whole page, and `heartbeat`; the gateway rotates it hourly with a clean EOF.
func (c *Client) StreamAISessions(ctx context.Context, q AISessionQuery) (*Stream, error) {
	q.Page = 0
	return c.Stream(ctx, aiSessionsStreamPath, q.values(), "")
}

// StreamAIGitActivities opens a session's live git feed of `upsert` frames keyed by activity_id.
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
	return &Error{Code: detail.Code, Message: detail.Message}
}

// AIPrincipalQuery selects principals; Filter is AIP-160 over kind and source_system.
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

// AllAIPrincipals follows every page of the catalog, which is bounded per tenant.
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
