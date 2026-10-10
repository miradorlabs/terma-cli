package main

import (
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// A key a failed run of a build saw as text, and its whole census only as a number, is not
// withheld: the whole run's kinds replace the failed run's, and a failed run after adds none.
func TestAWholeRunsKindsReplaceAFailedRuns(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	failed := map[string]bool{"codex\x000.164.0": true}
	text := field("0.164.0", "logs/codex.api_request", "attempt", "unclassified", day)
	number := field("0.164.0", "logs/codex.api_request", "attempt", "unclassified", day.Add(24*time.Hour), e2e.KindNumber)
	for _, order := range []string{"failed first", "whole first"} {
		var cat Catalog
		if order == "failed first" {
			mergeFields(&cat, []e2e.FieldRow{text}, failed)
			mergeFields(&cat, []e2e.FieldRow{number}, nil)
		} else {
			mergeFields(&cat, []e2e.FieldRow{number}, nil)
			mergeFields(&cat, []e2e.FieldRow{text}, failed)
		}
		if f := cat.Fields[0]; !slices.Equal(f.Kinds, []string{e2e.KindNumber}) || f.withheld() {
			t.Errorf("%s: kinds %v, withheld %v", order, f.Kinds, f.withheld())
		}
	}
	// Nor does a failed run of a newer build.
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{number}, nil)
	newer := field("0.165.0", "logs/codex.api_request", "attempt", "unclassified", day.Add(48*time.Hour))
	mergeFields(&cat, []e2e.FieldRow{newer}, map[string]bool{"codex\x000.165.0": true})
	if f := cat.Fields[0]; f.withheld() {
		t.Errorf("a newer build's failed run: kinds %v", f.Kinds)
	}
}

// A failed run's class, and what the relay keeps, carry through: they are the terma under
// test's answer, not the harness's, so a key it classified since is no longer withheld.
func TestAFailedRunsClassCarriesThrough(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.164.0", "logs/codex.api_request", "k", "unclassified", day)}, nil)
	for _, v := range []string{"0.164.0", "0.165.0"} {
		mergeFields(&cat, []e2e.FieldRow{field(v, "logs/codex.api_request", "k", "safe", day.Add(24*time.Hour))}, map[string]bool{"codex\x00" + v: true})
		if f := cat.Fields[0]; f.Class != "safe" || f.withheld() {
			t.Errorf("after a failed run of %s: class %s, withheld %v", v, f.Class, f.withheld())
		}
	}
}

// A key's kinds follow the newest build a whole census saw it in, whatever order the runs came
// in: an older build's whole census after a newer build's failed run, and one night's builds
// written in string order (2.0.10 before 2.0.9).
func TestKindsFollowTheNewestWholeBuildInAnyOrder(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	row := func(v string, n int, kinds ...string) e2e.FieldRow {
		return field(v, "logs/codex.api_request", "k", "unclassified", day.Add(time.Duration(n)*24*time.Hour), kinds...)
	}
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{row("0.163.0", 0), row("0.164.0", 0)}, map[string]bool{"codex\x000.163.0": true, "codex\x000.164.0": true})
	mergeFields(&cat, []e2e.FieldRow{row("0.163.0", 1, e2e.KindNumber), row("0.164.0", 1)}, map[string]bool{"codex\x000.164.0": true})
	if f := cat.Fields[0]; !slices.Equal(f.Kinds, []string{e2e.KindNumber}) || f.withheld() {
		t.Errorf("an outage, then a whole older build: kinds %v", f.Kinds)
	}
	for _, rows := range [][]e2e.FieldRow{
		{row("2.0.10", 0, e2e.KindNumber), row("2.0.9", 0)},
		{row("2.0.9", 0), row("2.0.10", 0, e2e.KindNumber)},
	} {
		var cat Catalog
		mergeFields(&cat, rows, map[string]bool{"codex\x002.0.10": true})
		if f := cat.Fields[0]; !slices.Equal(f.Kinds, []string{e2e.KindText}) || !f.withheld() {
			t.Errorf("rows from %s first: kinds %v, want 2.0.9's whole census's", rows[0].Version, f.Kinds)
		}
	}
}
