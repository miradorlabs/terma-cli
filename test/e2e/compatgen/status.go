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
	// Untested is a newer build than Version tonight's rows name whose every check was
	// skipped; UntestedNew says it is tested for the first time.
	Untested    string `json:"untested,omitempty"`
	UntestedNew bool   `json:"untested_new,omitempty"`
	// Regressed says a check Version passed before failed tonight (CompatChange); Fixed, that
	// it failed checks before and every one of them passed tonight (the history), none left
	// unchecked.
	Regressed bool `json:"regressed,omitempty"`
	Fixed     bool `json:"fixed,omitempty"`
}

// Passed reports a build that ran checks and failed none.
func (s Status) Passed() bool { return s.Checks > 0 && len(s.Failed) == 0 }

// statuses are the night's harnesses, failing first, then by name: each one's newest build
// tonight's rows name that ran checks (a newer one whose checks were all skipped is named
// beside it), with its checks, its release where it is one, and whether it changed from
// before: a regression from compat, a fix from the history before tonight (hist).
func statuses(rows []e2e.CompatRow, rs []Release, compat []CompatChange, hist map[string]Entry) []Status {
	type build struct{ harness, version string }
	checks := map[build]map[FailedCheck]bool{}
	failed := map[build][]FailedCheck{}
	passed := map[build]map[FailedCheck]bool{}
	newest, newestRun := map[string]string{}, map[string]string{}
	for _, r := range rows {
		b := build{r.Harness, r.Version}
		if v, ok := newest[r.Harness]; !ok || versionLess(v, r.Version) {
			newest[r.Harness] = r.Version
		}
		if r.Result == "not run" {
			continue
		}
		if v, ok := newestRun[r.Harness]; !ok || versionLess(v, r.Version) {
			newestRun[r.Harness] = r.Version
		}
		if checks[b] == nil {
			checks[b] = map[FailedCheck]bool{}
		}
		c := FailedCheck{Capability: r.Capability, Platform: r.Platform}
		checks[b][c] = true
		if r.Result == "fail" && !slices.Contains(failed[b], c) {
			failed[b] = append(failed[b], c)
		}
		if r.Result == "pass" {
			if passed[b] == nil {
				passed[b] = map[FailedCheck]bool{}
			}
			passed[b][c] = true
		}
	}
	var out []Status
	for harness, latest := range newest {
		v := latest
		if run, ok := newestRun[harness]; ok {
			v = run
		}
		b := build{harness, v}
		s := Status{Harness: harness, Name: harnessName(harness), Version: v, Checks: len(checks[b]), Failed: failed[b]}
		if latest != v {
			s.Untested = latest
			s.UntestedNew = slices.ContainsFunc(rs, func(r Release) bool { return r.Harness == harness && r.Version == latest })
		}
		for _, c := range compat {
			if c.Harness == harness && c.Version == v {
				s.Regressed = s.Regressed || c.From == "pass" && c.To == "fail"
			}
		}
		// Fixed only where every check it failed before passed tonight: one skipped tonight
		// may still fail.
		var before []FailedCheck
		for _, e := range hist {
			if e.Harness == harness && e.Version == v && e.Result == "fail" {
				before = append(before, FailedCheck{Capability: e.Capability, Platform: e.Platform})
			}
		}
		s.Fixed = len(s.Failed) == 0 && len(before) > 0 && !slices.ContainsFunc(before, func(c FailedCheck) bool { return !passed[b][c] })
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
	// What is red first, so a long night never cuts it from the message.
	rank := func(s Status) int {
		switch {
		case len(s.Failed) > 0:
			return 0
		case s.Checks == 0:
			return 1
		}
		return 2
	}
	slices.SortFunc(out, func(a, b Status) int { return cmp.Or(rank(a)-rank(b), strings.Compare(a.Name, b.Name)) })
	return out
}

// line says a harness's verdict: green where its newest build sends all the telemetry terma
// expects, red where it does not, naming the checks it failed; strong makes text bold.
func (s Status) line(strong func(string) string) string {
	mark, verdict := "✅", fmt.Sprintf(": passes all %d checks", s.Checks)
	if s.Checks == 1 {
		verdict = ": passes its 1 check"
	}
	switch {
	case s.Checks == 0:
		mark, verdict = "⚪", ": no checks ran"
	case len(s.Failed) > 0:
		mark = "❌"
		verdict = fmt.Sprintf(": fails %d of %s: %s", len(s.Failed), plural(s.Checks, "check"), strings.Join(failedNames(s.Failed), "; "))
		if s.Regressed {
			verdict += " · newly failing, passed before"
		}
	case s.Fixed:
		verdict += " · fixed, failed before"
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
	switch {
	case s.Untested != "" && s.UntestedNew:
		line += " · " + s.Untested + " (new) ran no checks"
	case s.Untested != "":
		line += " · " + s.Untested + " ran no checks"
	}
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
// someone classifies them: a note, never a failure. Each harness's keys are sorted, at most
// maxListed of them.
func (d Drift) newFields() string {
	var parts []string
	for _, h := range d.Harnesses {
		var keys []string
		for _, c := range h.Withheld {
			if k := "`" + c.Key + "`"; !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		if len(keys) > maxListed {
			keys = append(keys[:maxListed], fmt.Sprintf("and %d more", len(keys)-maxListed))
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

// summary is the notification's text: whether every harness tested sends what terma expects,
// how many do not and how many ran no checks, or that none was tested.
func (d Drift) summary() string {
	failing, untested := 0, 0
	for _, s := range d.Status {
		switch {
		case len(s.Failed) > 0:
			failing++
		case s.Checks == 0:
			untested++
		}
	}
	harnesses := func(n int) string {
		if n == 1 {
			return "1 harness"
		}
		return fmt.Sprintf("%d harnesses", n)
	}
	switch {
	case len(d.Status) == 0:
		return "Harness drift: no harness was tested tonight"
	case untested == len(d.Status):
		return "Harness drift: no harness ran its checks tonight"
	case failing > 0 && untested > 0:
		return "Harness drift: " + harnesses(failing) + " not sending what terma expects, " + harnesses(untested) + " ran no checks"
	case failing > 0:
		return "Harness drift: " + harnesses(failing) + " not sending what terma expects"
	case untested > 0:
		return "Harness drift: " + harnesses(untested) + " ran no checks; the rest send what terma expects"
	}
	return "Harness drift: every harness sends what terma expects"
}
