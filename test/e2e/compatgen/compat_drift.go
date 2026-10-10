package main

import (
	"sort"

	"github.com/miradorlabs/terma-cli/e2e"
)

// CompatChange is a capability whose result for a build changed, or a build's first failure.
type CompatChange struct {
	Harness    string `json:"harness"`
	Version    string `json:"version"`
	Platform   string `json:"platform"`
	Capability string `json:"capability"`
	From       string `json:"from,omitempty"`
	To         string `json:"to"`
}

// compatDrift compares a night's results with the history before they are merged.
func compatDrift(hist map[string]Entry, rows []e2e.CompatRow) []CompatChange {
	var out []CompatChange
	for _, r := range rows {
		e := Entry{Harness: r.Harness, Version: r.Version, Platform: r.Platform, Capability: r.Capability}
		prev, ok := hist[e.key()]
		switch {
		case r.Result == "not run":
		case !ok && r.Result == "fail":
			out = append(out, CompatChange{r.Harness, r.Version, r.Platform, r.Capability, "", r.Result})
		case ok && prev.Result != "not run" && prev.Result != r.Result:
			out = append(out, CompatChange{r.Harness, r.Version, r.Platform, r.Capability, prev.Result, r.Result})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Harness+a.Version+a.Platform+a.Capability < b.Harness+b.Version+b.Platform+b.Capability
	})
	return out
}
