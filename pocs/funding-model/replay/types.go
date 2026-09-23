// Package replay compares captured calls with the actual grain and units of
// provider exports. It does not learn from the same period it is evaluating.
package replay

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

const MaxInput = 32 << 20

// Scope is an explicit binding to one organization's report. End is exclusive.
// Source records who/what established that binding, not a credential.
type Scope struct {
	Provider          string   `json:"provider"`
	OrganizationID    string   `json:"organization_id"`
	Start             string   `json:"start"`
	End               string   `json:"end"`
	Product           string   `json:"product"`
	Measure           string   `json:"measure"`  // net_usd, gross_usd, or credits
	Coverage          string   `json:"coverage"` // complete, partial, unknown
	Source            string   `json:"source"`
	CreditsUSDPerUnit *float64 `json:"credits_usd_per_unit,omitempty"`
	ValuationSource   string   `json:"valuation_source,omitempty"`
}

// Capture uses settled per-call usage, never cumulative session token counters.
// ReferenceUSD must come from telemetry or an explicitly priced backend export;
// the simulator's illustrative price table is never used by this package.
type Capture struct {
	Version        int     `json:"version"`
	OrganizationID string  `json:"organization_id"`
	Source         string  `json:"source"`
	Calls          []Call  `json:"calls"`
	Events         []Event `json:"events"`
}

type Call struct {
	ID           string        `json:"id"`
	SessionID    string        `json:"session_id"`
	UserID       string        `json:"user_id"`
	AccountID    string        `json:"account_id"`
	Harness      model.Harness `json:"harness"`
	Product      string        `json:"product"`
	Model        string        `json:"model"`
	At           time.Time     `json:"at"`
	ReferenceUSD *float64      `json:"reference_usd"`
	PriceSource  string        `json:"price_source"`
	AuthMode     string        `json:"auth_mode,omitempty"`
	Speed        string        `json:"speed,omitempty"`
	Entrypoint   string        `json:"entrypoint,omitempty"`
}

// Event is the production spool shape, also used for normalized delivered logs.
type Event struct {
	Time      time.Time                  `json:"time"`
	Name      string                     `json:"name"`
	SessionID string                     `json:"session_id"`
	Repo      string                     `json:"repo,omitempty"`
	Attrs     map[string]json.RawMessage `json:"attrs"`
}

type Row struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	AccountID string    `json:"account_id,omitempty"`
	Model     string    `json:"model,omitempty"`
	Amount    string    `json:"amount"`
}

type ProviderReport struct {
	SHA256       string `json:"sha256"`
	Unit         string `json:"unit"`
	Rows         []Row  `json:"rows"`
	ExcludedRows int    `json:"excluded_rows"`
}

type CallEstimate struct {
	ID                 string                    `json:"id"`
	SessionID          string                    `json:"session_id"`
	Funding            model.Funding             `json:"estimated_funding"`
	Probabilities      map[model.Funding]float64 `json:"heuristic_probabilities"`
	ExpectedMeteredUSD *float64                  `json:"expected_metered_usd"`
	Basis              string                    `json:"basis"`
	Warnings           []string                  `json:"warnings,omitempty"`
}

type Comparison struct {
	Row
	Calls         int      `json:"calls"`
	UnpricedCalls int      `json:"unpriced_calls"`
	EstimatedUSD  *float64 `json:"estimated_usage_credit_usd"`
	ReportedUSD   *float64 `json:"reported_usd"`
	DifferenceUSD *float64 `json:"estimated_minus_reported_usd"`
	Status        string   `json:"status"`
}

type Result struct {
	Version            int            `json:"version"`
	Scope              Scope          `json:"scope"`
	CaptureSource      string         `json:"capture_source"`
	ReportSHA256       string         `json:"report_sha256"`
	Unit               string         `json:"reported_unit"`
	Comparisons        []Comparison   `json:"comparisons"`
	Calls              []CallEstimate `json:"calls"`
	CallsWithoutReport int            `json:"calls_without_report"`
	DuplicateCalls     int            `json:"duplicate_calls_ignored"`
	ExcludedCalls      int            `json:"calls_outside_scope"`
	ExcludedReportRows int            `json:"report_rows_not_compared"`
	Warnings           []string       `json:"warnings"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func period(start, end string) (time.Time, time.Time, error) {
	a, e1 := time.Parse("2006-01-02", start)
	b, e2 := time.Parse("2006-01-02", end)
	if e1 != nil || e2 != nil || !a.Before(b) {
		return a, b, fmt.Errorf("period must be UTC dates with start < exclusive end")
	}
	return a, b, nil
}

func (s Scope) validate() error {
	if _, _, err := period(s.Start, s.End); err != nil {
		return err
	}
	if s.OrganizationID == "" || s.Product == "" || s.Source == "" {
		return fmt.Errorf("organization_id, product and source are required")
	}
	if s.Coverage != "complete" && s.Coverage != "partial" && s.Coverage != "unknown" {
		return fmt.Errorf("coverage must be complete, partial or unknown")
	}
	if (s.Provider != "claude" || (s.Measure != "net_usd" && s.Measure != "gross_usd")) && (s.Provider != "openai" || s.Measure != "credits") {
		return fmt.Errorf("unsupported provider/measure pair")
	}
	if s.CreditsUSDPerUnit != nil && (s.Provider != "openai" || !finite(*s.CreditsUSDPerUnit) || *s.CreditsUSDPerUnit == 0 || s.ValuationSource == "") {
		return fmt.Errorf("credit valuation requires a positive USD/unit value and valuation_source")
	}
	return nil
}

// ReadFile caps local input size. No file contents are included in errors.
func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("input must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxInput+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxInput {
		return nil, fmt.Errorf("input exceeds 32 MiB")
	}
	return b, nil
}
