package api

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The log store's query surface at /v1/logs, where a `terma.commit` record is read back by sha.
const logsQueryPath = "/v1/logs"

// LogRecord is one record from the log store, read through its typed accessors.
type LogRecord struct {
	EventName          string         `json:"event_name"`
	Body               string         `json:"body"`
	Time               time.Time      `json:"time"`
	Attributes         map[string]any `json:"attributes"`
	ResourceAttributes map[string]any `json:"resource_attributes"`
}

// Attr returns an event attribute as a string, or "" when absent; a number or bool is coerced.
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

// logsResponse is the query envelope; both "logs" and the older "records" are read.
type logsResponse struct {
	Logs    []LogRecord `json:"logs"`
	Records []LogRecord `json:"records"`
}

func (resp logsResponse) all() []LogRecord {
	out := make([]LogRecord, 0, len(resp.Logs)+len(resp.Records))
	out = append(out, resp.Logs...)
	return append(out, resp.Records...)
}

// LogQuery selects log records in [Since, Until); the store caps the span, so callers
// centre a tight window rather than scanning from now.
type LogQuery struct {
	Filter string
	Since  time.Time
	Until  time.Time
	Limit  int
}

func (q LogQuery) values() url.Values {
	v := url.Values{}
	if q.Filter != "" {
		v.Set("filter", q.Filter)
	}
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

// QueryLogs runs one log query and returns the matching records in the store's order.
func (c *Client) QueryLogs(ctx context.Context, q LogQuery) ([]LogRecord, error) {
	var resp logsResponse
	if err := c.Get(ctx, logsQueryPath, q.values(), &resp); err != nil {
		return nil, err
	}
	return resp.all(), nil
}

// CommitLog fetches the terma.commit record for sha in [since, until), or (nil, nil) when
// the store holds none.
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

func logQuote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}
