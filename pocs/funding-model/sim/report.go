package sim

import (
	"math"
	"sort"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

type agg struct {
	requests int
	tokens   model.Tokens
	spend    float64
}

// buildReports renders the providers' exports from the truth, under their
// documented scopes: the Claude Team spend report meters usage-credit spend
// only, Console usage meters API-key spend at list, the OpenAI credits export
// meters Codex credit spend. Gateway and cloud routes produce no provider
// export: that bill is the customer's own and never reaches us this way.
func buildReports(w *World) []model.ReportRow {
	acc := map[model.ReportKind]map[model.DayKey]*agg{
		model.ReportClaudeTeamSpend:    {},
		model.ReportClaudeConsoleUsage: {},
		model.ReportOpenAICredits:      {},
	}
	scored := w.Base.AddDate(0, 0, w.Scenario.WarmupDays)
	for _, c := range w.Calls {
		if c.At.Before(scored) {
			continue
		}
		t := w.Truth[c.ID]
		var kind model.ReportKind
		switch {
		case c.Harness == model.HarnessClaude && t.Route == model.RouteSubscription && w.Sessions[c.SessionID].Account.Tier() == "max_5x":
			// An individual plan has no organisation export.
			continue
		case c.Harness == model.HarnessClaude && t.Route == model.RouteSubscription:
			kind = model.ReportClaudeTeamSpend
		case c.Harness == model.HarnessClaude && t.Route == model.RouteAPIKey:
			kind = model.ReportClaudeConsoleUsage
		case c.Harness == model.HarnessCodex && t.Route == model.RouteSubscription:
			kind = model.ReportOpenAICredits
		default:
			continue
		}
		k := model.DayKey{UserID: c.UserID, Model: c.Model, Day: model.Day(c.At)}
		a := acc[kind][k]
		if a == nil {
			a = &agg{}
			acc[kind][k] = a
		}
		a.requests++
		a.tokens.Add(c.Tokens)
		a.spend += t.MeteredUSD
	}
	var rows []model.ReportRow
	for kind, m := range acc {
		for k, a := range m {
			spend := cents(a.spend)
			if kind == model.ReportClaudeTeamSpend && spend == 0 && !w.Scenario.ReportIncludesAllowanceRows {
				continue
			}
			rows = append(rows, model.ReportRow{Kind: kind, UserID: k.UserID, Model: k.Model, Day: k.Day,
				Requests: a.requests, Tokens: a.tokens, SpendUSD: spend})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if !a.Day.Equal(b.Day) {
			return a.Day.Before(b.Day)
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		return a.Model < b.Model
	})
	return rows
}

func cents(x float64) float64 { return math.Round(x*100) / 100 }
