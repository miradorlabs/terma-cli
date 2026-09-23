package replay

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

// Run compares subscription usage-credit estimates at the export's own grain.
// It never allocates the entire bill to observed calls or treats absent rows as
// zero. API metering remains in the per-call output, outside these seat exports.
func Run(scope Scope, csv []byte, capture Capture) (Result, error) {
	out := Result{Version: 1, Scope: scope, CaptureSource: capture.Source, Calls: []CallEstimate{}, Comparisons: []Comparison{}, Warnings: []string{
		"Funding probabilities are uncalibrated heuristics, not measured confidence.",
		"Comparisons cover estimated subscription usage credits only; API charges and seat fees need separate reports.",
		"Report totals may include activity outside this capture; differences also include pricing and discount differences.",
		"Missing rows are unknown, never zero; report coverage does not establish telemetry coverage.",
	}}
	report, err := ParseReport(csv, scope)
	if err != nil {
		return out, err
	}
	if capture.Version != 1 || capture.OrganizationID != scope.OrganizationID || capture.Source == "" {
		return out, fmt.Errorf("capture requires version 1, a source, and organization_id matching the report scope")
	}
	out.ReportSHA256, out.Unit, out.ExcludedReportRows = report.SHA256, report.Unit, report.ExcludedRows
	start, end, _ := period(scope.Start, scope.End)
	harness := model.HarnessClaude
	if scope.Provider == "openai" {
		harness = model.HarnessCodex
	}
	events := map[string][]Event{}
	seenEvents := map[string]Event{}
	for i, ev := range capture.Events {
		if ev.Time.IsZero() || ev.SessionID == "" || ev.Name == "" {
			return out, fmt.Errorf("event %d lacks time, session_id or name", i+1)
		}
		tool := str(ev.Attrs, "tool")
		eventKey := tool + "\x00" + ev.SessionID + "\x00" + ev.Name + "\x00" + ev.Time.UTC().Format(time.RFC3339Nano)
		if previous, ok := seenEvents[eventKey]; ok {
			if !reflect.DeepEqual(previous.Attrs, ev.Attrs) {
				return out, fmt.Errorf("event %d conflicts with another snapshot at the same time", i+1)
			}
			continue
		}
		seenEvents[eventKey] = ev
		events[tool+"\x00"+ev.SessionID] = append(events[tool+"\x00"+ev.SessionID], ev)
	}
	seen := map[string]Call{}
	calls := []Call{}
	for i, c := range capture.Calls {
		if c.ID == "" || c.UserID == "" || c.SessionID == "" || c.Model == "" || c.Product == "" || c.At.IsZero() || (c.Harness != model.HarnessClaude && c.Harness != model.HarnessCodex) {
			return out, fmt.Errorf("call %d lacks required identity, time or harness", i+1)
		}
		if c.ReferenceUSD != nil && (!finite(*c.ReferenceUSD) || c.PriceSource == "") {
			return out, fmt.Errorf("call %d requires a nonnegative reference_usd and price_source", i+1)
		}
		if prev, ok := seen[c.ID]; ok {
			if !reflect.DeepEqual(prev, c) {
				return out, fmt.Errorf("call %d conflicts with an earlier call ID", i+1)
			}
			out.DuplicateCalls++
			continue
		}
		seen[c.ID] = c
		if c.Harness != harness || c.Product != scope.Product || c.At.Before(start) || !c.At.Before(end) {
			out.ExcludedCalls++
			continue
		}
		calls = append(calls, c)
	}
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].At.Equal(calls[j].At) {
			return calls[i].ID < calls[j].ID
		}
		return calls[i].At.Before(calls[j].At)
	})
	// No illustrative simulator prices or allowance capacities enter this run.
	estimator := model.New(model.Config{})
	groups := map[string][]int{}
	for i, row := range report.Rows {
		cmp := Comparison{Row: row, Status: "unmatched_report"}
		v, _ := amount(row.Amount)
		if report.Unit == "USD" {
			cmp.ReportedUSD = &v
		} else if scope.CreditsUSDPerUnit != nil {
			v *= *scope.CreditsUSDPerUnit
			if !finite(v) {
				return out, fmt.Errorf("credit valuation overflow")
			}
			cmp.ReportedUSD = &v
		}
		out.Comparisons = append(out.Comparisons, cmp)
		key := row.AccountID + "\x00" + row.Model
		groups[key] = append(groups[key], i)
	}
	for _, c := range calls {
		s, q, warnings := sessionAt(c, events[string(c.Harness)+"\x00"+c.SessionID], scope.OrganizationID)
		account := c.AccountID
		if account == "" {
			account = "user:" + c.UserID
		}
		s.UserID = string(c.Harness) + "\x00" + account
		mc := model.Call{ID: c.ID, SessionID: c.SessionID, UserID: s.UserID, Harness: c.Harness, At: c.At, Model: c.Model, Speed: c.Speed, Entrypoint: c.Entrypoint, AuthMode: c.AuthMode, Quota: q}
		if c.ReferenceUSD != nil {
			mc.ReferenceUSD = *c.ReferenceUSD
		}
		e := estimator.Estimate(s, mc)
		ce := CallEstimate{ID: c.ID, SessionID: c.SessionID, Funding: e.Best(), Probabilities: e.P, Basis: e.Basis, Warnings: warnings}
		if c.ReferenceUSD != nil {
			ce.ExpectedMeteredUSD = &e.ExpectedMeteredUSD
		} else {
			ce.Warnings = append(ce.Warnings, "missing reference price; monetary estimate withheld")
		}
		key := c.AccountID + "\x00" + c.Model
		if scope.Provider == "openai" {
			key = "\x00"
		}
		match := -1
		for _, i := range groups[key] {
			r := out.Comparisons[i]
			if !c.At.Before(r.Start) && c.At.Before(r.End) {
				match = i
				break
			}
		}
		if match < 0 {
			out.CallsWithoutReport++
			ce.Warnings = append(ce.Warnings, "no matching report row")
		} else {
			cmp := &out.Comparisons[match]
			cmp.Calls++
			if c.ReferenceUSD == nil {
				cmp.UnpricedCalls++
			} else {
				if cmp.EstimatedUSD == nil {
					cmp.EstimatedUSD = new(float64)
				}
				*cmp.EstimatedUSD += e.P[model.FundingCredits] * *c.ReferenceUSD
				if !finite(*cmp.EstimatedUSD) {
					return out, fmt.Errorf("estimated amount overflow")
				}
			}
		}
		out.Calls = append(out.Calls, ce)
	}
	for i := range out.Comparisons {
		cmp := &out.Comparisons[i]
		switch {
		case cmp.Calls == 0: // Keep reported spend visible without assigning it to calls.
		case cmp.UnpricedCalls > 0:
			cmp.Status = "unpriced_calls"
			cmp.EstimatedUSD = nil
		case cmp.ReportedUSD == nil:
			cmp.Status = "unvalued_credits"
		default:
			cmp.Status = "compared"
			d := *cmp.EstimatedUSD - *cmp.ReportedUSD
			cmp.DifferenceUSD = &d
		}
	}
	if report.Unit == "credits" && scope.CreditsUSDPerUnit == nil {
		out.Warnings = append(out.Warnings, "Codex credits have no USD valuation; raw credits are retained and no dollar difference is computed.")
	}
	if len(report.Rows) == 0 {
		out.Warnings = append(out.Warnings, "No report rows cover the selected scope.")
	}
	if len(calls) == 0 {
		out.Warnings = append(out.Warnings, "No captured calls cover the selected scope.")
	}
	return out, nil
}
