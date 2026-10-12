package main

import "github.com/miradorlabs/terma-cli/e2e"

// What the night's census scenarios ran (report/census.json), and the builds of the night's
// rows: what fieldDrift judges a census whole by, and a build reached.

// censusRuns is what tonight's census scenarios did (e2e.CensusRun, report/census.json): per
// harness, the newest build they ran, passed or failed; and the builds whose census is
// partial, one of them failed for, or did not run.
type censusRuns struct {
	newest map[string]string
	failed map[string]bool // harness and version
}

// censusRan reads the night's census runs.
func censusRan(runs []e2e.CensusRun) censusRuns {
	ran := censusRuns{newest: map[string]string{}, failed: map[string]bool{}}
	scenarios := map[string]map[string]bool{} // harness → the census scenarios that ran it tonight
	ranBuild := map[string]map[string]bool{}  // harness and version → the scenarios that ran it
	for _, r := range runs {
		b := r.Harness + "\x00" + r.Version
		if scenarios[r.Harness] == nil {
			scenarios[r.Harness] = map[string]bool{}
		}
		if ranBuild[b] == nil {
			ranBuild[b] = map[string]bool{}
		}
		scenarios[r.Harness][r.Scenario], ranBuild[b][r.Scenario] = true, true
		if versionLess(ran.newest[r.Harness], r.Version) {
			ran.newest[r.Harness] = r.Version
		}
		if r.Failed {
			ran.failed[b] = true
		}
	}
	// A build a census scenario of the night did not run (a release between two scenarios'
	// lookups, a build one could not fetch) lacks that scenario's surfaces: partial.
	for _, r := range runs {
		if b := r.Harness + "\x00" + r.Version; len(ranBuild[b]) < len(scenarios[r.Harness]) {
			ran.failed[b] = true
		}
	}
	return ran
}

// firstOr is vs's first, or none.
func firstOr(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

// rowBuilds are the builds rows are of.
func rowBuilds(rows []e2e.FieldRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Version] = true
	}
	return out
}
