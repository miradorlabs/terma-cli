package main

import (
	"cmp"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
)

// Releases are the harness builds a night ran that no night before it did: whether each
// passed its checks, the capabilities the live suite proved of it, and what its census found
// new or gone, so a release is said once, the night it is first tested.

// Release is one harness build the night ran for the first time.
type Release struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Previous is the newest build of the harness known before tonight, older than Version;
	// Compare links the source's changes between them, where it is public (linkSources).
	Previous string `json:"previous,omitempty"`
	Compare  string `json:"compare,omitempty"`
	// Checks counts the capability checks the build ran tonight, each capability on each
	// platform once; Failed are those that failed. A check skipped is not counted.
	Checks int           `json:"checks"`
	Failed []FailedCheck `json:"failed,omitempty"`
	// Fields is what its census found, nil where the night took no census of it.
	Fields *ReleaseFields `json:"fields,omitempty"`
}

// FailedCheck is a capability a release failed on one platform.
type FailedCheck struct {
	Capability string `json:"capability"`
	Platform   string `json:"platform"`
}

// ReleaseFields counts what a release's census found, as the harness's drift says it of
// that build.
type ReleaseFields struct {
	First       bool `json:"first,omitempty"`
	Partial     bool `json:"partial,omitempty"`
	Surfaces    int  `json:"surfaces,omitempty"`
	Keys        int  `json:"keys,omitempty"`
	NewSurfaces int  `json:"new_surfaces,omitempty"`
	Added       int  `json:"added,omitempty"`
	Removed     int  `json:"removed,omitempty"`
	Unseen      int  `json:"unseen,omitempty"`
	Withheld    int  `json:"withheld,omitempty"`
}

// Passed reports a release that ran checks and failed none.
func (r Release) Passed() bool { return r.Checks > 0 && len(r.Failed) == 0 }

// releases are the builds tonight ran for the first time that are newer than any run before,
// by harness then version. A build ran on a night that has a result of it, passed or failed,
// or a census of it; one whose every check was skipped did not, so it is said neither then nor
// known after, and its first night that runs is its release. A build older than the newest
// known is no release: the catalog forgets old censuses, and a build it forgot, run again, is
// no news (nor is a backport). hs is the night's field drift, and ran what its census
// scenarios ran.
func releases(hist map[string]Entry, cat Catalog, rows []e2e.CompatRow, fields []e2e.FieldRow, hs []HarnessDrift, ran censusRuns) []Release {
	known := map[string][]string{} // harness → builds before tonight
	for _, e := range hist {
		if e.Result != "not run" {
			known[e.Harness] = append(known[e.Harness], e.Version)
		}
	}
	for _, c := range cat.Censuses {
		known[c.Harness] = append(known[c.Harness], c.Version)
	}
	type build struct{ harness, version string }
	checks := map[build]map[FailedCheck]bool{} // the checks each build ran, passed or failed
	failed := map[build][]FailedCheck{}
	var builds []build
	newest := map[string]string{}
	for h, vs := range known {
		newest[h] = slices.MaxFunc(vs, compareVersions)
	}
	add := func(b build) {
		if versionLess(newest[b.harness], b.version) && !slices.Contains(builds, b) {
			builds = append(builds, b)
		}
	}
	for _, r := range rows {
		if r.Result == "not run" {
			continue
		}
		b := build{r.Harness, r.Version}
		add(b)
		if checks[b] == nil {
			checks[b] = map[FailedCheck]bool{}
		}
		c := FailedCheck{Capability: r.Capability, Platform: r.Platform}
		checks[b][c] = true
		if r.Result == "fail" && !slices.Contains(failed[b], c) {
			failed[b] = append(failed[b], c)
		}
	}
	for _, f := range fields {
		add(build{f.Harness, f.Version})
	}
	slices.SortFunc(builds, func(a, b build) int {
		return cmp.Or(strings.Compare(a.harness, b.harness), compareVersions(a.version, b.version))
	})
	var out []Release
	for _, b := range builds {
		r := Release{Harness: b.harness, Name: harnessName(b.harness), Version: b.version, Checks: len(checks[b]), Failed: failed[b]}
		for _, v := range known[b.harness] {
			if versionLess(v, b.version) && versionLess(r.Previous, v) {
				r.Previous = v
			}
		}
		slices.SortFunc(r.Failed, func(x, y FailedCheck) int {
			return cmp.Or(strings.Compare(x.Capability, y.Capability), strings.Compare(x.Platform, y.Platform))
		})
		for _, h := range hs {
			if h.Harness == b.harness {
				r.Fields = h.fieldsOf(b.version, fields, ran)
			}
		}
		out = append(out, r)
	}
	return out
}

// compareVersions orders two builds as versionLess does, 0 for the same.
func compareVersions(a, b string) int {
	switch {
	case versionLess(a, b):
		return -1
	case versionLess(b, a):
		return 1
	}
	return 0
}

// fieldsOf counts what h says of build v, nil where the night took no census of it. What came
// is the build's that first sent it tonight, the oldest: h's From names the newest, and a key
// two of tonight's builds send is the first one's news. What went is the build's In names.
func (h HarnessDrift) fieldsOf(v string, rows []e2e.FieldRow, ran censusRuns) *ReleaseFields {
	first := map[string]string{} // surface, or surface and key → the oldest build tonight that sent it
	surfaces, keys := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		if r.Harness != h.Harness {
			continue
		}
		for _, id := range []string{r.Surface, fieldID(r.Surface, r.Key)} {
			if was, ok := first[id]; !ok || versionLess(r.Version, was) {
				first[id] = r.Version
			}
		}
		if r.Version == v {
			surfaces[r.Surface], keys[fieldID(r.Surface, r.Key)] = true, true
		}
	}
	if len(keys) == 0 {
		return nil
	}
	n := night{harness: h.Harness, ran: ran}
	f := &ReleaseFields{First: h.First, Partial: n.partial(v), Surfaces: len(surfaces), Keys: len(keys)}
	gone := func(in string) bool { return in == v || in == "" && h.lastJudged() == v }
	for _, s := range h.NewSurfaces {
		if first[s.Surface] == v {
			f.NewSurfaces++
		}
	}
	for _, c := range h.Added {
		if first[fieldID(c.Surface, c.Key)] == v {
			f.Added++
		}
	}
	for _, c := range h.Withheld {
		if first[fieldID(c.Surface, c.Key)] == v {
			f.Withheld++
		}
	}
	for _, c := range h.Removed {
		if gone(c.In) {
			f.Removed++
		}
	}
	for _, s := range h.Unseen {
		if gone(s.In) {
			f.Unseen++
		}
	}
	return f
}

// otherCompat are the compatibility changes no verdict says: a harness's newest build tonight
// has its own line (Status), which names each check it fails, newly or not, and says it fixed
// only where every earlier failure passed; a fix beside a failure it still has is said here.
// A release's first failures are said on its line or among the older builds failing.
func (d Drift) otherCompat() []CompatChange {
	return slices.DeleteFunc(slices.Clone(d.Compat), func(c CompatChange) bool {
		if i := slices.IndexFunc(d.Status, func(s Status) bool { return s.Harness == c.Harness && s.Version == c.Version }); i >= 0 {
			return !(c.From == "fail" && c.To == "pass" && !d.Status[i].Fixed)
		}
		return c.From == "" && slices.ContainsFunc(d.Releases, func(r Release) bool { return r.Harness == c.Harness && r.Version == c.Version })
	})
}
