package model

import (
	"math"
	"testing"
	"time"
)

func TestReconcileAllocatesReportedSpendByExpectedCredits(t *testing.T) {
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	calls := []Call{
		{ID: "a", UserID: "u", Harness: HarnessClaude, At: day.Add(9 * time.Hour), Model: "m", ReferenceUSD: 1, Speed: "normal"},
		{ID: "b", UserID: "u", Harness: HarnessClaude, At: day.Add(10 * time.Hour), Model: "m", ReferenceUSD: 1, Speed: "normal"},
		{ID: "x", UserID: "u", Harness: HarnessCodex, At: day.Add(10 * time.Hour), Model: "m", ReferenceUSD: 5, AuthMode: "swic"},
	}
	ests := map[string]Estimate{
		"a": {CallID: "a", P: map[Funding]float64{FundingIncluded: 0.9, FundingCredits: 0.1}, PRoute: map[Route]float64{RouteSubscription: 1}},
		"b": {CallID: "b", P: map[Funding]float64{FundingIncluded: 0.1, FundingCredits: 0.9}, PRoute: map[Route]float64{RouteSubscription: 1}},
		"x": {CallID: "x", P: map[Funding]float64{FundingIncluded: 1}, PRoute: map[Route]float64{RouteSubscription: 1}},
	}
	rows := []ReportRow{
		{Kind: ReportClaudeTeamSpend, UserID: "u", Model: "m", Day: day, Requests: 2, SpendUSD: 1.00},
		{Kind: ReportClaudeTeamSpend, UserID: "other", Model: "m", Day: day, Requests: 3, SpendUSD: 0.40},
	}
	res := Reconcile(ReportClaudeTeamSpend, rows, calls, ests)
	if len(res.Rows) != 1 {
		t.Fatalf("codex call must not join a Claude report: %d rows", len(res.Rows))
	}
	r := res.Rows[0]
	if !r.HasRow || r.ReportedUSD != 1 || math.Abs(r.EstimatedUSD-1.0) > 1e-9 {
		t.Fatalf("row: %+v", r)
	}
	if math.Abs(r.Allocated["a"]-0.1) > 1e-9 || math.Abs(r.Allocated["b"]-0.9) > 1e-9 {
		t.Fatalf("allocation should follow expected credits: %+v", r.Allocated)
	}
	if math.Abs(res.UnmatchedUSD-0.40) > 1e-9 {
		t.Fatalf("spend for a user we saw no calls from must be unmatched, got %.2f", res.UnmatchedUSD)
	}
}

func TestReconcileTreatsMissingRowAsZeroAndLearnTightensPrior(t *testing.T) {
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	calls := []Call{{ID: "a", UserID: "u", Harness: HarnessClaude, At: day.Add(9 * time.Hour), Model: "m", ReferenceUSD: 4, Speed: "normal"}}
	ests := map[string]Estimate{"a": {CallID: "a", P: map[Funding]float64{FundingIncluded: 0.5, FundingCredits: 0.5}, PRoute: map[Route]float64{RouteSubscription: 1}}}
	// A user the export never mentions is not covered by it: no rows, nothing learned.
	if res := Reconcile(ReportClaudeTeamSpend, nil, calls, ests); len(res.Rows) != 0 {
		t.Fatalf("uncovered user must not be reconciled: %+v", res.Rows)
	}
	// Covered elsewhere in the export, silent on this day: that day is zero.
	other := []ReportRow{{Kind: ReportClaudeTeamSpend, UserID: "u", Model: "m", Day: day.AddDate(0, 0, 1), Requests: 3}}
	res := Reconcile(ReportClaudeTeamSpend, other, calls, ests)
	if len(res.Rows) != 1 || res.Rows[0].HasRow || res.Rows[0].ReportedUSD != 0 || res.Rows[0].EstimatedUSD != 2 {
		t.Fatalf("missing row: %+v", res.Rows)
	}
	e := New(Config{})
	e.Learn(res)
	pr := e.prior("u")
	if pr.CreditsB != 1 || pr.CreditsA != 0 {
		t.Fatalf("a day with no credit spend is one observation of share 0: %+v", pr)
	}
}

func TestSetPriorsCopies(t *testing.T) {
	a := New(Config{})
	a.prior("u").addRoute(RouteAPIKey, 2)
	b := New(Config{})
	b.SetPriors(a.Priors())
	b.prior("u").addRoute(RouteAPIKey, 1)
	if a.prior("u").routeObs(RouteAPIKey) != 2 || b.prior("u").routeObs(RouteAPIKey) != 3 {
		t.Fatal("SetPriors must copy, not alias")
	}
}
