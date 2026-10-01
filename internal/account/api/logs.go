package api

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The log store's query surface lives at /v1/logs — the same door the OTLP exporter
// posts to, read back with a filter. It is how a commit is joined to the session that
// produced it: the post-commit hook exports a `terma.commit` record carrying the
// commit sha and the stamped session id, and this reads it back by sha.
const logsQueryPath = "/v1/logs"

// LogRecord is one record from the log store. Every attribute value comes back as a
// string, nested under "attributes" (the event's own) and "resource_attributes" (the
// exporter's); the typed accessors read those rather than exposing the raw maps.
type LogRecord struct {
	EventName          string         `json:"event_name"`
	Body               string         `json:"body"`
	Time               time.Time      `json:"time"`
	Attributes         map[string]any `json:"attributes"`
	ResourceAttributes map[string]any `json:"resource_attributes"`
}

// Attr returns an event attribute as a string, or "" when absent. Values arrive as
// strings today; a number or bool is coerced rather than dropped, so a future typed
// encoding does not silently read as empty.
func (r *LogRecord) Attr(key string) string { return coerceAttr(r.Attributes[key]) }

// ResourceAttr is Attr for the exporter's resource attributes.
func (r *LogRecord) ResourceAttr(key string) string { return coerceAttr(r.ResourceAttributes[key]) }

// Int returns an integer attribute, or 0 when absent or unparseable.
func (r *LogRecord) Int(key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(r.Attr(key)))
	return n
}

func coerceAttr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// logsResponse is the query envelope. The store names the array "logs"; an earlier
// build named it "records", so both are read and concatenated.
type logsResponse struct {
	Logs    []LogRecord `json:"logs"`
	Records []LogRecord `json:"records"`
}

func (resp logsResponse) all() []LogRecord {
	out := make([]LogRecord, 0, len(resp.Logs)+len(resp.Records))
	out = append(out, resp.Logs...)
	return append(out, resp.Records...)
}

// LogQuery selects log records. Filter is the store's expression over attribute.<key>,
// status, severity and tag; the window is [Since, Until) and the store caps its span
// (currently 840h), so callers centre a tight window rather than scanning from now.
type LogQuery struct {
	Filter string
	Since  time.Time
	Until  time.Time
	Limit  int
}

func (q LogQuery) values() url.Values {
	v := url.Values{}
	setIfNotEmpty(v, "filter", q.Filter)
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	if !q.Until.IsZero() {
		v.Set("until", q.Until.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

// QueryLogs runs one log query and returns the matching records, as the store orders
// them. An empty match is an empty slice, not an error.
func (c *Client) QueryLogs(ctx context.Context, q LogQuery) ([]LogRecord, error) {
	var resp logsResponse
	if err := c.Get(ctx, logsQueryPath, q.values(), &resp); err != nil {
		return nil, err
	}
	return resp.all(), nil
}

// CommitLog fetches the terma.commit record a connected agent reported for one commit
// sha, searching the [since, until) window. It returns (nil, nil) when the store holds
// no such record — the commit carried no Agent-Session-Id trailer, or its event has
// not reached the backend yet.
func (c *Client) CommitLog(ctx context.Context, sha string, since, until time.Time) (*LogRecord, error) {
	recs, err := c.QueryLogs(ctx, LogQuery{
		Filter: `attribute.event.name="terma.commit" AND attribute.sha=` + logQuote(sha),
		Since:  since,
		Until:  until,
		Limit:  1,
	})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// logQuote wraps a value for the log filter grammar, escaping the two characters that
// would end the quoted string or the escape itself.
func logQuote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}
