package cursorbilling

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

func (c *Client) Spend(ctx context.Context) (SpendSnapshot, error) {
	out := SpendSnapshot{Scope: c.scope, StartedAt: time.Now().UTC(), Members: []Spend{}, RawPages: []json.RawMessage{}}
	total, pages := -1, -1
	seen := map[string]bool{}
	for page := 1; page <= c.maxPages; page++ {
		raw, err := c.request(ctx, "/teams/spend", map[string]any{"page": page, "pageSize": c.pageSize, "sortBy": "user", "sortDirection": "asc"})
		if err != nil {
			return SpendSnapshot{}, err
		}
		var data struct {
			Rows  *[]json.RawMessage `json:"teamMemberSpend"`
			Total *int               `json:"totalMembers"`
			Pages *int               `json:"totalPages"`
			Cycle *Millis            `json:"subscriptionCycleStart"`
		}
		if err = decode(raw, &data); err != nil || data.Rows == nil || data.Total == nil || data.Pages == nil || data.Cycle == nil || *data.Total < 0 || *data.Pages < 0 || *data.Pages > c.maxPages {
			return SpendSnapshot{}, fmt.Errorf("Cursor spend response lacks valid pagination or cycle")
		}
		if page == 1 {
			if *data.Total > c.maxRecords || (*data.Total > 0 && *data.Pages == 0) {
				return SpendSnapshot{}, fmt.Errorf("Cursor spend exceeds record bound or contradicts page count")
			}
			total, pages = *data.Total, *data.Pages
			out.CycleStart = *data.Cycle
		}
		if *data.Total != total || *data.Pages != pages || *data.Cycle != out.CycleStart {
			return SpendSnapshot{}, fmt.Errorf("Cursor spend changed during pagination; retry whole snapshot")
		}
		if len(*data.Rows) > c.pageSize || len(out.Members)+len(*data.Rows) > total || (len(*data.Rows) == 0 && total > 0) {
			return SpendSnapshot{}, fmt.Errorf("Cursor spend pagination returned invalid page size")
		}
		for _, r := range *data.Rows {
			var m Spend
			if err = decode(r, &m); err != nil {
				return SpendSnapshot{}, err
			}
			if m.UserID == "" || seen[m.UserID] {
				return SpendSnapshot{}, fmt.Errorf("Cursor spend user missing or repeated across pages")
			}
			seen[m.UserID] = true
			m.Raw = append(json.RawMessage(nil), r...)
			out.Members = append(out.Members, m)
		}
		out.Pages = page
		out.RawPages = append(out.RawPages, append(json.RawMessage(nil), raw...))
		if page >= pages {
			if len(out.Members) != total {
				return SpendSnapshot{}, fmt.Errorf("Cursor spend total does not match collected rows")
			}
			out.ObservedAt = time.Now().UTC()
			return out, nil
		}
	}
	return SpendSnapshot{}, fmt.Errorf("Cursor spend exceeded page bound")
}

// Usage returns only a fully traversed snapshot. It preserves identical rows:
// the provider may legitimately produce them, and no documented unique ID is
// available here. Replace a fixed window atomically; never append poll results.
func (c *Client) Usage(ctx context.Context, w Window) (UsageSnapshot, error) {
	if err := w.validate(); err != nil {
		return UsageSnapshot{}, err
	}
	out := UsageSnapshot{Scope: c.scope, Window: w, StartedAt: time.Now().UTC(), Events: []UsageEvent{}, Warnings: []string{
		"Pagination is not snapshot-isolated; stable counts cannot exclude every concurrent provider change.",
		"Complete means all reported pages were read; provider reporting lag and later corrections remain possible.",
		"Content fingerprints are not event IDs. Replace fixed windows; do not add overlapping snapshots.",
	}}
	total, pages, pageSize := -1, -1, -1
	pageHashes := map[string]bool{}
	for page := 1; page <= c.maxPages; page++ {
		raw, err := c.request(ctx, "/teams/filtered-usage-events", map[string]any{"page": page, "pageSize": c.pageSize, "startDate": w.Start.UnixMilli(), "endDate": w.End.UnixMilli() - 1})
		if err != nil {
			return UsageSnapshot{}, err
		}
		var data struct {
			Rows       *[]json.RawMessage `json:"usageEvents"`
			Total      *int               `json:"totalUsageEventsCount"`
			Pagination *struct {
				Current *int  `json:"currentPage"`
				Pages   *int  `json:"numPages"`
				Size    *int  `json:"pageSize"`
				Next    *bool `json:"hasNextPage"`
			} `json:"pagination"`
			Period *struct {
				Start *Millis `json:"startDate"`
				End   *Millis `json:"endDate"`
			} `json:"period"`
		}
		if err = decode(raw, &data); err != nil {
			return UsageSnapshot{}, err
		}
		p := data.Pagination
		if data.Rows == nil || data.Total == nil || *data.Total < 0 || p == nil || p.Current == nil || *p.Current != page || p.Pages == nil || *p.Pages < 0 || *p.Pages > c.maxPages || p.Next == nil || p.Size == nil || *p.Size < 1 || *p.Size > 1000 {
			return UsageSnapshot{}, fmt.Errorf("Cursor usage response lacks valid pagination")
		}
		if data.Period == nil || data.Period.Start == nil || data.Period.End == nil || int64(*data.Period.Start) != w.Start.UnixMilli() || int64(*data.Period.End) != w.End.UnixMilli()-1 {
			return UsageSnapshot{}, fmt.Errorf("Cursor usage response period does not match requested window")
		}
		if page == 1 {
			if *data.Total > c.maxRecords || (*data.Total > 0 && *p.Pages == 0) {
				return UsageSnapshot{}, fmt.Errorf("Cursor usage exceeds record bound or contradicts page count")
			}
			total, pages, pageSize = *data.Total, *p.Pages, *p.Size
		}
		if *data.Total != total || *p.Pages != pages || *p.Size != pageSize || *p.Next != (page < pages) {
			return UsageSnapshot{}, fmt.Errorf("Cursor usage changed or contradicted pagination; retry whole window")
		}
		if len(*data.Rows) > pageSize || len(out.Events)+len(*data.Rows) > total || (len(*data.Rows) == 0 && total > 0) {
			return UsageSnapshot{}, fmt.Errorf("Cursor usage pagination returned invalid page size")
		}
		hashes := []string{}
		for _, r := range *data.Rows {
			var event UsageEvent
			if err = decode(r, &event); err != nil {
				return UsageSnapshot{}, err
			}
			if event.Timestamp == nil || int64(*event.Timestamp) < w.Start.UnixMilli() || int64(*event.Timestamp) >= w.End.UnixMilli() {
				return UsageSnapshot{}, fmt.Errorf("Cursor usage event timestamp outside window or missing")
			}
			if u := event.TokenUsage; u != nil {
				for _, n := range []*int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens} {
					if n != nil && *n < 0 {
						return UsageSnapshot{}, fmt.Errorf("Cursor usage has negative token count")
					}
				}
			}
			normalized, err := canonical(r)
			if err != nil {
				return UsageSnapshot{}, err
			}
			event.Raw = append(json.RawMessage(nil), r...)
			event.Fingerprint = digest(normalized)
			hashes = append(hashes, event.Fingerprint)
			out.Events = append(out.Events, event)
		}
		sorted := append([]string(nil), hashes...)
		sort.Strings(sorted)
		b, _ := json.Marshal(sorted)
		h := digest(b)
		if len(hashes) > 0 && pageHashes[h] {
			return UsageSnapshot{}, fmt.Errorf("Cursor usage repeated a page; ambiguous pagination, no snapshot returned")
		}
		pageHashes[h] = true
		out.Pages = page
		if !*p.Next {
			if len(out.Events) != total {
				return UsageSnapshot{}, fmt.Errorf("Cursor usage total does not match collected rows")
			}
			fingerprints := make([]string, len(out.Events))
			for i, e := range out.Events {
				fingerprints[i] = e.Fingerprint
			}
			sort.Strings(fingerprints)
			b, _ = json.Marshal(struct {
				Scope      Scope
				Start, End int64
				Records    []string
			}{c.scope, w.Start.UnixMilli(), w.End.UnixMilli(), fingerprints})
			out.ContentID = digest(b)
			out.Complete = true
			out.ObservedAt = time.Now().UTC()
			return out, nil
		}
	}
	return UsageSnapshot{}, fmt.Errorf("Cursor usage exceeded page bound")
}
