package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
)

var decimal = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

func amount(s string) (float64, error) {
	if len(s) > 128 || !decimal.MatchString(s) {
		return 0, fmt.Errorf("amount must be a nonnegative decimal; missing values and adjustments require separate handling")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("invalid decimal")
	}
	v, _ := r.Float64()
	if !finite(v) {
		return 0, fmt.Errorf("amount is out of range")
	}
	return v, nil
}

// ParseReport retains the observed schemas' grain: Claude account/model/period
// USD and OpenAI workspace/product/interval credits. Neither becomes user/day USD.
func ParseReport(data []byte, scope Scope) (ProviderReport, error) {
	out := ProviderReport{SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Unit: "USD", Rows: []Row{}}
	if err := scope.validate(); err != nil {
		return out, err
	}
	if len(data) > MaxInput {
		return out, fmt.Errorf("report exceeds 32 MiB")
	}
	if scope.Provider == "openai" {
		out.Unit = "credits"
		if scope.Product != "Codex" {
			return out, fmt.Errorf("this pilot compares the Codex product only")
		}
	}
	reader := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	records, err := reader.ReadAll()
	if err != nil || len(records) < 2 {
		return out, fmt.Errorf("report must be a CSV header followed by rows")
	}
	ix := map[string]int{}
	for i, k := range records[0] {
		if _, ok := ix[k]; ok {
			return out, fmt.Errorf("duplicate CSV column")
		}
		ix[k] = i
	}
	required := []string{"account_uuid", "product", "model", "total_net_spend_usd", "total_gross_spend_usd"}
	if scope.Provider == "openai" {
		required = []string{"Start Time", "End Time", "Codex", "Work", "Chat"}
	}
	for _, k := range required {
		if _, ok := ix[k]; !ok {
			return out, fmt.Errorf("report missing required column %s", k)
		}
	}
	start, end, _ := period(scope.Start, scope.End)
	for line, values := range records[1:] {
		get := func(k string) string { return strings.TrimSpace(values[ix[k]]) }
		row := Row{Start: start, End: end}
		if scope.Provider == "claude" {
			if get("product") != scope.Product {
				out.ExcludedRows++
				continue
			}
			row.AccountID, row.Model = get("account_uuid"), get("model")
			if row.AccountID == "" || row.Model == "" {
				return out, fmt.Errorf("row %d lacks account/model identity", line+2)
			}
			key := "total_net_spend_usd"
			if scope.Measure == "gross_usd" {
				key = "total_gross_spend_usd"
			}
			row.Amount = get(key)
		} else {
			row.Start, row.End, err = period(get("Start Time"), get("End Time"))
			if err != nil {
				return out, fmt.Errorf("row %d: %w", line+2, err)
			}
			if !row.End.After(start) || !row.Start.Before(end) {
				out.ExcludedRows++
				continue
			}
			if row.Start.Before(start) || row.End.After(end) {
				return out, fmt.Errorf("row %d overlaps the period boundary; cannot prorate credits", line+2)
			}
			row.Amount = get("Codex")
			if row.Amount == "" {
				out.ExcludedRows++
				continue
			} // no value is not zero
		}
		if _, err := amount(row.Amount); err != nil {
			return out, fmt.Errorf("row %d: %w", line+2, err)
		}
		out.Rows = append(out.Rows, row)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Start.Before(b.Start)
	})
	for i := 1; i < len(out.Rows); i++ {
		a, b := out.Rows[i-1], out.Rows[i]
		if a.AccountID == b.AccountID && a.Model == b.Model && b.Start.Before(a.End) {
			return out, fmt.Errorf("duplicate or overlapping aggregate rows; resolve their scope before reconciling")
		}
	}
	return out, nil
}
