package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The night's verdict on each harness, the first thing the digest says: does its newest build
// tested tonight send all the telemetry terma expects? It does where every capability check it
// ran passed. A field the relay has not classified is no failure (the nightly notes it,
// TERMA_E2E_UNCLASSIFIED=note); the digest says it under what is to note.

// Status is how a harness's newest build tested tonight did.
type Status struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Checks counts the capability checks it ran, each capability on each platform once;
	// Failed are those that failed. A check skipped is not counted.
	Checks int           `json:"checks"`
	Failed []FailedCheck `json:"failed,omitempty"`
	// Release is set where Version is tested for the first time tonight.
	Release *Release `json:"release,omitempty"`
	// OlderFailed are the older builds tested tonight that failed a check.
	OlderFailed []string `json:"older_failed,omitempty"`
}

// Passed reports a build that ran checks and failed none.
func (s Status) Passed() bool { return s.Checks > 0 && len(s.Failed) == 0 }

// statuses are the night's harnesses, by name: each one's newest build tonight's rows name,
// with its checks, and its release where it is one.
func statuses(rows []e2e.CompatRow, rs []Release) []Status {
	type build struct{ harness, version string }
	checks := map[build]map[FailedCheck]bool{}
	failed := map[build][]FailedCheck{}
	newest := map[string]string{}
	for _, r := range rows {
		b := build{r.Harness, r.Version}
		if v, ok := newest[r.Harness]; !ok || versionLess(v, r.Version) {
			newest[r.Harness] = r.Version
		}
		if r.Result == "not run" {
			continue
		}
		if checks[b] == nil {
			checks[b] = map[FailedCheck]bool{}
		}
		c := FailedCheck{Capability: r.Capability, Platform: r.Platform}
		checks[b][c] = true
		if r.Result == "fail" && !slices.Contains(failed[b], c) {
			failed[b] = append(failed[b], c)
		}
	}
	var out []Status
	for harness, v := range newest {
		b := build{harness, v}
		s := Status{Harness: harness, Name: harnessName(harness), Version: v, Checks: len(checks[b]), Failed: failed[b]}
		slices.SortFunc(s.Failed, func(x, y FailedCheck) int {
			return cmp.Or(strings.Compare(x.Capability, y.Capability), strings.Compare(x.Platform, y.Platform))
		})
		if i := slices.IndexFunc(rs, func(r Release) bool { return r.Harness == harness && r.Version == v }); i >= 0 {
			s.Release = &rs[i]
		}
		for f := range failed {
			if f.harness == harness && f.version != v && !slices.Contains(s.OlderFailed, f.version) {
				s.OlderFailed = append(s.OlderFailed, f.version)
			}
		}
		slices.SortFunc(s.OlderFailed, compareVersions)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// line says a harness's verdict: green where its newest build sends all the telemetry terma
// expects, red where it does not, naming the checks it failed; strong makes text bold.
func (s Status) line(strong func(string) string) string {
	mark, verdict := "✅", ""
	switch {
	case s.Checks == 0:
		mark, verdict = "⚪", ": no checks ran"
	case len(s.Failed) > 0:
		mark = "❌"
		verdict = fmt.Sprintf(": fails %d of %s: %s", len(s.Failed), plural(s.Checks, "check"), strings.Join(failedNames(s.Failed), "; "))
	}
	build := strong(s.Name + " " + s.Version)
	if r := s.Release; r != nil {
		switch {
		case r.Previous != "" && r.Compare != "":
			build += " · new (was " + r.Previous + ", " + link(r.Compare, "diff") + ")"
		case r.Previous != "":
			build += " · new (was " + r.Previous + ")"
		default:
			build += " · new"
		}
	}
	line := mark + " " + build + verdict
	if len(s.OlderFailed) > 0 {
		line += " · older builds failing: " + strings.Join(s.OlderFailed, ", ")
	}
	return line
}

// failedNames names failed checks, at most three and a count of the rest: a platform by its
// short name, unless two of them share one (linux/amd64 and linux/arm64), which then name the
// architecture.
func failedNames(failed []FailedCheck) []string {
	short := map[string][]string{}
	for _, c := range failed {
		if s := platformShort(c.Platform); !slices.Contains(short[s], c.Platform) {
			short[s] = append(short[s], c.Platform)
		}
	}
	var names []string
	for _, c := range failed {
		where := platformShort(c.Platform)
		if len(short[where]) > 1 {
			where = c.Platform
		}
		names = append(names, capabilityLabel(c.Capability)+" ("+where+")")
	}
	if len(names) > 3 {
		names = append(names[:3], fmt.Sprintf("+%d more", len(names)-3))
	}
	return names
}

// statusLines are the night's verdicts, with strong for bold.
func (d Drift) statusLines(strong func(string) string) []string {
	var out []string
	for _, s := range d.Status {
		out = append(out, s.line(strong))
	}
	return out
}

// newFields names, per harness, the fields new tonight that the relay withholds until
// someone classifies them: a note, never a failure.
func (d Drift) newFields() string {
	var parts []string
	for _, h := range d.Harnesses {
		var keys []string
		for _, c := range h.Withheld {
			if k := "`" + c.Key + "`"; !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 {
			parts = append(parts, h.Name+": "+strings.Join(keys, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "New fields, withheld until classified: " + strings.Join(parts, "; ") + "."
}
