package sim

import (
	"reflect"
	"testing"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

func TestGenerateIsDeterministic(t *testing.T) {
	sc, _ := Preset("mixed-org")
	a, b := Generate(sc, 7), Generate(sc, 7)
	if !reflect.DeepEqual(a.Calls, b.Calls) || !reflect.DeepEqual(a.Reports, b.Reports) {
		t.Fatal("same scenario and seed must give the same world")
	}
	if c := Generate(sc, 8); reflect.DeepEqual(a.Calls, c.Calls) {
		t.Fatal("different seeds must differ")
	}
}

func TestTruthRespectsProviderRules(t *testing.T) {
	for _, sc := range Presets {
		w := Generate(sc, 3)
		if len(w.Calls) == 0 {
			t.Fatalf("%s: no calls", sc.Name)
		}
		for _, c := range w.Calls {
			tr := w.Truth[c.ID]
			sess := w.Sessions[c.SessionID]
			if sess.ID == "" {
				t.Fatalf("%s: call %s has no session", sc.Name, c.ID)
			}
			switch {
			case c.Speed == "fast" && tr.Funding != model.FundingCredits:
				t.Fatalf("%s: fast call %s funded %s", sc.Name, c.ID, tr.Funding)
			case tr.Funding == model.FundingCredits && !sess.Account.CreditsAvailable() && sc.SnapshotPerTurn:
				t.Fatalf("%s: credits without credits available on %s", sc.Name, c.ID)
			case tr.Route != model.RouteSubscription && tr.Funding != model.FundingMetered:
				t.Fatalf("%s: non-subscription route %s funded %s", sc.Name, tr.Route, tr.Funding)
			case tr.Funding.Metered() != (tr.MeteredUSD > 0):
				t.Fatalf("%s: funding %s with metered %.4f", sc.Name, tr.Funding, tr.MeteredUSD)
			}
		}
	}
}

func TestReportsSumToTruth(t *testing.T) {
	sc, _ := Preset("team-credits")
	w := Generate(sc, 5)
	var reported, truth float64
	for _, r := range w.Reports {
		if r.Kind == model.ReportClaudeTeamSpend {
			reported += r.SpendUSD
		}
	}
	scored := w.Base.AddDate(0, 0, sc.WarmupDays)
	for _, c := range w.Calls {
		if !c.At.Before(scored) {
			truth += w.Truth[c.ID].MeteredUSD
		}
	}
	if d := reported - truth; d > 0.01*float64(len(w.Reports)) || d < -0.01*float64(len(w.Reports)) {
		t.Fatalf("export %.2f vs truth %.2f: rounding alone cannot explain this", reported, truth)
	}
	if truth == 0 {
		t.Fatal("team-credits must produce credit spend or it tests nothing")
	}
}

func TestSparseExportOmitsZeroRows(t *testing.T) {
	sc, _ := Preset("team-credits-sparse-export")
	for _, r := range Generate(sc, 2).Reports {
		if r.Kind == model.ReportClaudeTeamSpend && r.SpendUSD == 0 {
			t.Fatal("sparse export must not list zero-spend days")
		}
	}
}
