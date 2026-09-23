package replay

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

func fixture(t *testing.T, provider string) (Scope, []byte, Capture) {
	t.Helper()
	read := func(path string) []byte {
		b, err := os.ReadFile("testdata/" + path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var s Scope
	var c Capture
	if err := json.Unmarshal(read(provider+"-scope.json"), &s); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read("capture.json"), &c); err != nil {
		t.Fatal(err)
	}
	return s, read(provider + ".csv"), c
}
func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestClaudeActualGrainAndFutureEvidence(t *testing.T) {
	s, csv, c := fixture(t, "claude")
	r, err := Run(s, csv, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Comparisons) != 2 || r.ExcludedReportRows != 1 || len(r.Calls) != 1 {
		t.Fatalf("scope lost: %+v", r)
	}
	row := r.Comparisons[0]
	if row.End.Sub(row.Start) != 72*time.Hour || row.Status != "compared" || row.Calls != 1 || *row.ReportedUSD != 0.85 || *row.EstimatedUSD < 0.8 || *row.EstimatedUSD > 0.9 {
		t.Fatalf("unexpected comparison: %+v", row)
	}
	if r.Calls[0].Funding != model.FundingCredits || r.Calls[0].Basis != "quota" {
		t.Fatalf("future disabled policy leaked: %+v", r.Calls[0])
	}
	if r.Comparisons[1].Status != "unmatched_report" || r.Comparisons[1].DifferenceUSD != nil {
		t.Fatal("unobserved spend allocated to captured calls")
	}
	// Remove the post-call update: estimates must be identical.
	c.Events = c.Events[:len(c.Events)-1]
	without, err := Run(s, csv, c)
	if err != nil {
		t.Fatal(err)
	}
	if *without.Comparisons[0].EstimatedUSD != *row.EstimatedUSD {
		t.Fatal("future evidence affected estimate")
	}
}

func TestCodexCreditsNeedExplicitValuation(t *testing.T) {
	s, csv, c := fixture(t, "openai")
	r, err := Run(s, csv, c)
	if err != nil {
		t.Fatal(err)
	}
	row := r.Comparisons[0]
	if r.Unit != "credits" || row.Amount != "12.3456" || row.Status != "unvalued_credits" || row.ReportedUSD != nil || row.DifferenceUSD != nil {
		t.Fatalf("invented USD: %+v", row)
	}
	if len(r.Comparisons) != 2 || r.Comparisons[1].Amount != "0" {
		t.Fatal("blank cell confused with explicit zero")
	}
	rate := 0.25
	s.CreditsUSDPerUnit = &rate
	if _, err := Run(s, csv, c); err == nil {
		t.Fatal("accepted valuation without provenance")
	}
	s.ValuationSource = "Synthetic test rate, not a provider price"
	r, err = Run(s, csv, c)
	if err != nil {
		t.Fatal(err)
	}
	if *r.Comparisons[0].ReportedUSD != 12.3456*rate || r.Comparisons[0].DifferenceUSD == nil {
		t.Fatal("explicit valuation not applied")
	}
}

func TestCaptureIntegrityAndMissingCoverage(t *testing.T) {
	t.Run("organization", func(t *testing.T) {
		s, b, c := fixture(t, "claude")
		c.OrganizationID = "other"
		if _, err := Run(s, b, c); err == nil {
			t.Fatal("cross-org capture accepted")
		}
	})
	t.Run("dedup", func(t *testing.T) {
		s, b, c := fixture(t, "claude")
		c.Calls = append(c.Calls, c.Calls[0])
		r, err := Run(s, b, c)
		if err != nil || r.DuplicateCalls != 1 || r.Comparisons[0].Calls != 1 {
			t.Fatalf("dedup: %+v %v", r, err)
		}
		c.Calls[2].SessionID = "different"
		if _, err := Run(s, b, c); err == nil {
			t.Fatal("conflicting call accepted")
		}
	})
	t.Run("price", func(t *testing.T) {
		s, b, c := fixture(t, "claude")
		c.Calls[0].ReferenceUSD = nil
		r, err := Run(s, b, c)
		if err != nil {
			t.Fatal(err)
		}
		if r.Comparisons[0].Status != "unpriced_calls" || r.Comparisons[0].EstimatedUSD != nil || r.Comparisons[0].DifferenceUSD != nil || r.Calls[0].ExpectedMeteredUSD != nil {
			t.Fatal("unpriced call became free usage")
		}
	})
	t.Run("missing row", func(t *testing.T) {
		s, b, c := fixture(t, "claude")
		c.Calls[0].Model = "absent"
		r, err := Run(s, b, c)
		if err != nil || r.CallsWithoutReport != 1 || r.Comparisons[0].Status != "unmatched_report" {
			t.Fatalf("missing row: %+v %v", r, err)
		}
	})
	t.Run("exclusive end", func(t *testing.T) {
		s, b, c := fixture(t, "claude")
		c.Calls[0].At = time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
		r, err := Run(s, b, c)
		if err != nil || len(r.Calls) != 0 {
			t.Fatalf("boundary: %+v %v", r, err)
		}
	})
}

func TestReportRejectsAmbiguousGrain(t *testing.T) {
	s, b, _ := fixture(t, "openai")
	for _, tc := range []struct{ name, csv string }{
		{"duplicate", string(b) + "2026-09-01,2026-09-02,2,0,0\n"},
		{"overlap", string(b) + "2026-09-01,2026-09-03,2,0,0\n"},
		{"boundary", string(b) + "2026-09-03,2026-09-05,2,0,0\n"},
		{"negative", strings.Replace(string(b), "12.3456", "-1", 1)},
		{"malformed", strings.Replace(string(b), "12.3456", "NaN", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseReport([]byte(tc.csv), s); err == nil {
				t.Fatal("ambiguous report accepted")
			}
		})
	}
}

func TestEvidenceExpiryIdentityAndTombstones(t *testing.T) {
	for _, name := range []string{"account switch", "org switch", "missing account", "expired quota", "stale quota", "spend cap only", "quota predates account"} {
		t.Run(name, func(t *testing.T) {
			s, _, c := fixture(t, "claude")
			call := c.Calls[0]
			c.Events = c.Events[:2]
			switch name {
			case "account switch":
				c.Events[0].Attrs["account_id"] = raw("other")
			case "org switch":
				c.Events[0].Attrs["organization_id"] = raw("other")
			case "missing account":
				e := c.Events[0]
				e.Time = call.At.Add(-time.Second)
				e.Attrs = map[string]json.RawMessage{"tool": raw("claude-code"), "evidence_status": raw("missing")}
				c.Events = append(c.Events, e)
			case "expired quota":
				c.Events[1].Attrs["five_hour_resets_at"] = raw(call.At.Unix())
			case "stale quota":
				call.At = call.At.Add(16 * time.Minute)
			case "spend cap only":
				delete(c.Events[1].Attrs, "five_hour_used_pct")
			case "quota predates account":
				c.Events[0].Time = call.At.Add(-time.Second)
			}
			_, q, _ := sessionAt(call, c.Events, s.OrganizationID)
			if q != nil {
				t.Fatalf("invalid quota used: %+v", q)
			}
		})
	}
	t.Run("codex source future", func(t *testing.T) {
		s, _, c := fixture(t, "openai")
		c.Events[2].Attrs["source_time"] = raw("2026-09-01T10:03:00Z")
		a, q, _ := sessionAt(c.Calls[1], c.Events, s.OrganizationID)
		if a.Account != nil || q != nil {
			t.Fatal("future source accepted")
		}
	})
	t.Run("codex missing supersedes", func(t *testing.T) {
		s, _, c := fixture(t, "openai")
		e := c.Events[2]
		e.Time = c.Calls[1].At.Add(-time.Second)
		e.Attrs = map[string]json.RawMessage{"tool": raw("codex"), "evidence_status": raw("unavailable")}
		c.Events = append(c.Events, e)
		a, q, _ := sessionAt(c.Calls[1], c.Events, s.OrganizationID)
		if a.Account != nil || q != nil {
			t.Fatal("old credits survived unavailable snapshot")
		}
	})
}

func TestConflictingSameTimeEvidenceFails(t *testing.T) {
	s, b, c := fixture(t, "claude")
	e := c.Events[0]
	e.Attrs = map[string]json.RawMessage{"tool": raw("claude-code"), "evidence_status": raw("missing")}
	c.Events = append(c.Events, e)
	if _, err := Run(s, b, c); err == nil {
		t.Fatal("conflicting evidence order silently chose a winner")
	}
}

func TestQuotaSourceOrderBreaksObservationTimeTies(t *testing.T) {
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	call := Call{SessionID: "s", Harness: model.HarnessCodex, At: at.Add(time.Second)}
	event := func(offset int, status string) Event {
		return Event{Time: at, Name: "terma.session.quota", SessionID: "s", Attrs: map[string]json.RawMessage{
			"tool": raw("codex"), "source_stream": raw("stream"), "source_offset": raw(offset),
			"source_time": raw(at.Format(time.RFC3339Nano)), "evidence_status": raw(status),
			"plan_type": raw("team"), "primary_used_pct": raw(98), "primary_resets_at": raw(at.Add(time.Hour).Unix()),
		}}
	}
	for _, events := range [][]Event{{event(1, "present"), event(2, "unavailable")}, {event(2, "unavailable"), event(1, "present")}} {
		_, q, _ := sessionAt(call, events, "")
		if q != nil {
			t.Fatal("older populated snapshot overrode newer unavailable observation")
		}
	}
	a, b := event(1, "present"), event(2, "present")
	delete(a.Attrs, "source_offset")
	delete(b.Attrs, "source_offset")
	a.Attrs["observation_sequence"] = raw(1)
	b.Attrs["observation_sequence"] = raw(2)
	if !laterEvidence(&b, &a) || laterEvidence(&a, &b) {
		t.Fatal("Claude sequence ordering lost")
	}
}
