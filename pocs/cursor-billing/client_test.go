package cursorbilling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testScope = Scope{TenantID: "tenant-a", ConnectionID: "connection-a"}
var testWindow = Window{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}

func testClient(t *testing.T, h http.HandlerFunc, modify func(*Config)) *Client {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	cfg := Config{APIKey: "test-private-key", Scope: testScope, BaseURL: server.URL, PageSize: 1}
	if modify != nil {
		modify(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func requestPage(t *testing.T, r *http.Request) int {
	t.Helper()
	user, pass, ok := r.BasicAuth()
	if !ok || user != "test-private-key" || pass != "" {
		t.Error("incorrect Basic authentication")
	}
	var req struct {
		Page       int `json:"page"`
		Start, End int64
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(fields["page"], &req.Page)
	if r.URL.Path == "/teams/filtered-usage-events" {
		_ = json.Unmarshal(fields["startDate"], &req.Start)
		_ = json.Unmarshal(fields["endDate"], &req.End)
		if req.Start != testWindow.Start.UnixMilli() || req.End != testWindow.End.UnixMilli()-1 {
			t.Error("inclusive provider boundary conversion is wrong")
		}
	}
	return req.Page
}
func event(i int) map[string]any {
	return map[string]any{"timestamp": fmt.Sprint(testWindow.Start.UnixMilli() + int64(i)), "userEmail": "test@example.invalid", "conversationId": "conversation-a", "model": "test-model", "kind": "Included in Business", "isChargeable": false, "chargedCents": json.Number("0.1"), "tokenUsage": map[string]any{"inputTokens": 10, "outputTokens": 2, "cacheReadTokens": 0, "cacheWriteTokens": nil, "totalCents": json.Number("0.2")}, "extraFutureField": "preserved"}
}
func pageBody(page, total, size int, rows []map[string]any) map[string]any {
	pages := (total + size - 1) / size
	return map[string]any{"usageEvents": rows, "totalUsageEventsCount": total, "pagination": map[string]any{"currentPage": page, "numPages": pages, "pageSize": size, "hasNextPage": page < pages}, "period": map[string]any{"startDate": testWindow.Start.UnixMilli(), "endDate": testWindow.End.UnixMilli() - 1}}
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestUsagePaginationAndExactAmounts(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := requestPage(t, r)
		writeJSON(w, pageBody(p, 3, 1, []map[string]any{event(p)}))
	}, nil)
	snapshot, err := c.Usage(context.Background(), testWindow)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete || snapshot.Pages != 3 || len(snapshot.Events) != 3 {
		t.Fatalf("incomplete: %+v", snapshot)
	}
	if snapshot.Events[0].TokenUsage.CacheWriteTokens != nil || snapshot.Events[0].TokenUsage.CacheReadTokens == nil || *snapshot.Events[0].TokenUsage.CacheReadTokens != 0 {
		t.Fatal("null confused with zero")
	}
	if !strings.Contains(string(snapshot.Events[0].Raw), "extraFutureField") {
		t.Fatal("raw field lost")
	}
	joined, err := JoinSessions(snapshot, testScope, []Session{{ConversationID: "conversation-a", UserEmail: "test@example.invalid", TurnIDs: []string{"one", "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := joined.Sessions[0]
	if s.MatchedEvents != 3 || s.ReportedCharge.KnownCents != "0.3" || s.ModelReference.KnownCents != "0.6" || s.CursorFee.MissingEvents != 3 {
		t.Fatalf("totals: %+v", s)
	}
	again, err := c.Usage(context.Background(), testWindow)
	if err != nil || again.ContentID != snapshot.ContentID {
		t.Fatal("repeat import changed identity")
	}
}

func TestIdenticalEventsRetainMultiplicity(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, pageBody(1, 2, 2, []map[string]any{event(1), event(1)}))
	}, func(c *Config) { c.PageSize = 2 })
	snapshot, err := c.Usage(context.Background(), testWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) != 2 || snapshot.Events[0].Fingerprint != snapshot.Events[1].Fingerprint {
		t.Fatal("legitimate identical events collapsed")
	}
	j, err := JoinSessions(snapshot, testScope, []Session{{ConversationID: "conversation-a"}})
	if err != nil || j.Sessions[0].ReportedCharge.KnownCents != "0.2" {
		t.Fatal("multiplicity lost")
	}
}

func TestContentIdentityOrderScopeAndCorrections(t *testing.T) {
	reverse := false
	correct := false
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		a, b := event(1), event(2)
		if reverse {
			a, b = b, a
		}
		if correct {
			a["chargedCents"] = 0
		}
		writeJSON(w, pageBody(1, 2, 2, []map[string]any{a, b}))
	}, func(c *Config) { c.PageSize = 2 })
	a, err := c.Usage(context.Background(), testWindow)
	if err != nil {
		t.Fatal(err)
	}
	reverse = true
	b, err := c.Usage(context.Background(), testWindow)
	if err != nil || a.ContentID != b.ContentID {
		t.Fatal("page ordering changed content identity")
	}
	correct = true
	d, err := c.Usage(context.Background(), testWindow)
	if err != nil || a.ContentID == d.ContentID {
		t.Fatal("correction was ignored")
	}
	c.scope.ConnectionID = "other"
	e, err := c.Usage(context.Background(), testWindow)
	if err != nil || e.ContentID == d.ContentID {
		t.Fatal("scope not part of content identity")
	}
}

func TestUsageRejectsPartialOrAmbiguousSnapshots(t *testing.T) {
	for _, name := range []string{"page failure", "changed count", "repeated page", "wrong period", "missing pagination", "wrong next", "negative tokens", "fractional tokens", "bad decimal", "count mismatch"} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				p := requestPage(t, r)
				e := event(p)
				data := pageBody(p, 2, 1, []map[string]any{e})
				switch name {
				case "page failure":
					if p == 2 {
						w.WriteHeader(500)
						return
					}
				case "changed count":
					if p == 2 {
						data["totalUsageEventsCount"] = 3
					}
				case "repeated page":
					data["usageEvents"] = []map[string]any{event(1)}
				case "wrong period":
					data["period"] = map[string]any{"startDate": 0, "endDate": 1}
				case "missing pagination":
					delete(data, "pagination")
				case "wrong next":
					data["pagination"].(map[string]any)["hasNextPage"] = false
				case "negative tokens":
					e["tokenUsage"].(map[string]any)["inputTokens"] = -1
				case "fractional tokens":
					e["tokenUsage"].(map[string]any)["inputTokens"] = 1.5
				case "bad decimal":
					e["chargedCents"] = "NaN"
				case "count mismatch":
					data = pageBody(1, 3, 3, []map[string]any{e})
				}
				writeJSON(w, data)
			}, nil)
			got, err := c.Usage(context.Background(), testWindow)
			if err == nil || got.Complete || len(got.Events) > 0 {
				t.Fatalf("partial import escaped: complete=%v records=%d err=%v", got.Complete, len(got.Events), err)
			}
		})
	}
}

func TestEmptyAndMissingUsage(t *testing.T) {
	for _, missing := range []bool{false, true} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			data := pageBody(1, 0, 1, []map[string]any{})
			if missing {
				delete(data, "usageEvents")
			}
			writeJSON(w, data)
		}, nil)
		s, err := c.Usage(context.Background(), testWindow)
		if missing {
			if err == nil {
				t.Fatal("missing records became zero")
			}
		} else if err != nil || !s.Complete || len(s.Events) != 0 {
			t.Fatal("empty complete page failed")
		}
	}
}

func TestHTTPBoundariesAndRedaction(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(status)
				fmt.Fprint(w, "secret test-private-key private email")
			}, nil)
			_, err := c.Members(context.Background())
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != status || he.RetryAfter != 12*time.Second || he.Retryable() != (status == 429 || status >= 500) || calls != 1 {
				t.Fatalf("bad HTTP handling: %v calls=%d", err, calls)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatal("response content leaked")
			}
		})
	}
	t.Run("no redirects", func(t *testing.T) {
		var hit atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
		defer target.Close()
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }, nil)
		_, err := c.Members(context.Background())
		if err == nil || hit.Load() {
			t.Fatal("followed credential redirect")
		}
	})
	t.Run("body bound", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 100)) }, func(c *Config) { c.MaxResponseBytes = 20 })
		if _, err := c.Members(context.Background()); err == nil {
			t.Fatal("oversized body accepted")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled request reached server") }, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.Members(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	})
}

func TestSpendAndMembers(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/teams/members" {
			writeJSON(w, map[string]any{"teamMembers": []any{map[string]any{"id": "a", "email": "test@example.invalid", "role": "member", "isRemoved": false}}})
			return
		}
		p := requestPage(t, r)
		writeJSON(w, map[string]any{"teamMemberSpend": []any{map[string]any{"userId": fmt.Sprint(p), "billingTier": "TIER_UNKNOWN", "spendCents": json.Number("1.123456789"), "monthlyLimitDollars": nil, "apiPercentUsed": 107}}, "totalMembers": 2, "totalPages": 2, "subscriptionCycleStart": testWindow.Start.UnixMilli()})
	}, nil)
	m, err := c.Members(context.Background())
	if err != nil || len(m.Members) != 1 {
		t.Fatalf("members: %v", err)
	}
	s, err := c.Spend(context.Background())
	if err != nil || len(s.Members) != 2 || s.Pages != 2 {
		t.Fatalf("spend: %v", err)
	}
	if *s.Members[0].APIPercentUsed != "107" || s.Members[0].MonthlyLimitDollars != nil || s.Members[0].OverallSpendCents != nil || *s.Members[0].SpendCents != "1.123456789" {
		t.Fatal("spend semantics lost")
	}
}
func TestSpendChangingCycleAndDuplicateUsers(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			p := requestPage(t, r)
			id := fmt.Sprint(p)
			cycle := testWindow.Start.UnixMilli()
			if duplicate {
				id = "same"
			} else {
				cycle += int64(p)
			}
			writeJSON(w, map[string]any{"teamMemberSpend": []any{map[string]any{"userId": id}}, "totalMembers": 2, "totalPages": 2, "subscriptionCycleStart": cycle})
		}, nil)
		s, err := c.Spend(context.Background())
		if err == nil || len(s.Members) != 0 {
			t.Fatal("inconsistent spend snapshot returned")
		}
	}
}

func TestJoinIdentityMissingPricesAndScope(t *testing.T) {
	d := Decimal("0")
	ts := Millis(testWindow.Start.UnixMilli())
	snap := UsageSnapshot{Scope: testScope, Window: testWindow, Complete: true, Events: []UsageEvent{{Timestamp: &ts, ConversationID: "a", UserEmail: "person-a", ChargedCents: &d}, {Timestamp: &ts, ConversationID: "a", UserEmail: "other"}, {Timestamp: &ts, ConversationID: ""}}}
	joined, err := JoinSessions(snap, testScope, []Session{{ConversationID: "a", UserEmail: "person-a"}, {ConversationID: "absent"}})
	if err != nil {
		t.Fatal(err)
	}
	if joined.UnmatchedEvents != 2 || joined.Sessions[0].IdentityMismatches != 1 || joined.Sessions[0].ModelReference.MissingEvents != 1 || joined.Sessions[0].ReportedCharge.MissingEvents != 0 || joined.Sessions[1].Status != "not_observed" {
		t.Fatalf("join: %+v", joined)
	}
	if _, err := JoinSessions(snap, Scope{TenantID: "other", ConnectionID: testScope.ConnectionID}, nil); err == nil {
		t.Fatal("cross-tenant join accepted")
	}
	snap.Complete = false
	if _, err := JoinSessions(snap, testScope, nil); err == nil {
		t.Fatal("partial snapshot accepted")
	}
}

func TestDecimalPrecisionAndValidation(t *testing.T) {
	var a Amount
	for _, s := range []string{"0.123456789123456789", "0.000000000000000001", "-0.1", "1e-20"} {
		d := Decimal(s)
		if err := a.add(&d); err != nil {
			t.Fatal(err)
		}
	}
	if a.KnownCents != "0.02345678912345679001" {
		t.Fatalf("rounded: %s", a.KnownCents)
	}
	for _, s := range []string{`"NaN"`, `"1e99999999"`, `true`, `""`, `"1/2"`} {
		var d Decimal
		if json.Unmarshal([]byte(s), &d) == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestRecordAndPageBounds(t *testing.T) {
	for _, endpoint := range []string{"members", "spend", "usage"} {
		t.Run(endpoint, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch endpoint {
				case "members":
					writeJSON(w, map[string]any{"teamMembers": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}})
				case "spend":
					writeJSON(w, map[string]any{"teamMemberSpend": []any{map[string]any{"userId": "a"}}, "totalMembers": 2, "totalPages": 2, "subscriptionCycleStart": testWindow.Start.UnixMilli()})
				case "usage":
					writeJSON(w, pageBody(1, 2, 1, []map[string]any{event(1)}))
				}
			}, func(c *Config) { c.MaxRecords = 1 })
			var err error
			switch endpoint {
			case "members":
				_, err = c.Members(context.Background())
			case "spend":
				_, err = c.Spend(context.Background())
			case "usage":
				_, err = c.Usage(context.Background(), testWindow)
			}
			if err == nil {
				t.Fatal("record bound ignored")
			}
		})
	}
	for _, endpoint := range []string{"spend", "usage"} {
		t.Run("zero pages with rows "+endpoint, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if endpoint == "spend" {
					writeJSON(w, map[string]any{"teamMemberSpend": []any{map[string]any{"userId": "a"}}, "totalMembers": 1, "totalPages": 0, "subscriptionCycleStart": testWindow.Start.UnixMilli()})
				} else {
					data := pageBody(1, 1, 1, []map[string]any{event(1)})
					data["pagination"].(map[string]any)["numPages"] = 0
					writeJSON(w, data)
				}
			}, nil)
			var err error
			if endpoint == "spend" {
				_, err = c.Spend(context.Background())
			} else {
				_, err = c.Usage(context.Background(), testWindow)
			}
			if err == nil {
				t.Fatal("contradictory page count accepted")
			}
		})
	}
}

func TestInvalidWindowsDoNotRequest(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid window reached server") }, nil)
	for _, window := range []Window{
		{}, {Start: testWindow.End, End: testWindow.Start},
		{Start: testWindow.Start, End: testWindow.Start.Add(32 * 24 * time.Hour)},
		{Start: testWindow.Start.Add(time.Nanosecond), End: testWindow.End},
	} {
		if _, err := c.Usage(context.Background(), window); err == nil {
			t.Fatal("invalid window accepted")
		}
	}
}
