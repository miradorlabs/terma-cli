package main

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// Partial censuses neither push the last whole one out of the catalog nor hide a removal: a
// key the last whole census saw stays through five partial builds, and the next whole build
// without it reports it removed, once.
func TestPartialCensusesKeepTheWholeOne(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.160.0", "logs/codex.api_request", "model", "safe", day),
		field("0.160.0", "logs/codex.api_request", "retry_reason", "safe", day),
	}, nil)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	for i := 1; i <= keepCensuses; i++ {
		v := fmt.Sprintf("0.16%d.0", i)
		partial := map[string]bool{"codex\x00" + v: true}
		mergeFields(&cat, []e2e.FieldRow{field(v, "logs/codex.api_request", "model", "safe", night(i))}, partial)
	}
	if !slices.ContainsFunc(cat.Fields, func(f FieldEntry) bool { return f.Key == "retry_reason" }) {
		t.Fatal("partial censuses pruned a key the last whole census saw")
	}
	if whole, ok := cat.lastWhole("codex", "0.170.0"); !ok || whole.Version != "0.160.0" {
		t.Fatalf("last whole census %+v, %v", whole, ok)
	}
	tonight := []e2e.FieldRow{field("0.170.0", "logs/codex.api_request", "model", "safe", night(9))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.170.0"}})
	_, _, hs := fieldDrift(cat, tonight, ran, night(9))
	if len(hs[0].Removed) != 1 || hs[0].Removed[0].Key != "retry_reason" {
		t.Errorf("removed %+v, want retry_reason, judged against the last whole census", hs[0].Removed)
	}
	mergeFields(&cat, tonight, ran.failed)
	if _, _, hs = fieldDrift(cat, tonight, ran, night(10)); len(hs[0].Removed) != 0 {
		t.Errorf("a re-run of a build censused whole reported %+v removed again", hs[0].Removed)
	}
}

// A build censused partially one night and whole the next is judged on the night it is whole.
func TestAPartialBuildIsJudgedWhenWhole(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.161.0", "logs/codex.api_request", "model", "safe", day),
		field("0.161.0", "logs/codex.api_request", "retry_reason", "safe", day),
	}, nil)
	tonight := []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour))}
	failed := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.162.0", Failed: true}})
	if _, _, hs := fieldDrift(cat, tonight, failed, day.Add(24*time.Hour)); !hs[0].Partial || len(hs[0].Removed) != 0 {
		t.Fatalf("partial night: %+v", hs[0])
	}
	mergeFields(&cat, tonight, failed.failed)
	passed := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.162.0"}})
	if _, _, hs := fieldDrift(cat, tonight, passed, day.Add(48*time.Hour)); len(hs[0].Removed) != 1 {
		t.Errorf("whole night: removed %+v, want retry_reason", hs[0].Removed)
	}
}

// A key sent by the partial builds after the last whole census, and by more of them than a key
// keeps builds of, is still judged gone when a whole build no longer sends it; and the digest
// names the build it was judged against.
func TestARemovalAcrossPartialBuilds(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	both := func(v string, at time.Time) []e2e.FieldRow {
		return []e2e.FieldRow{field(v, "logs/codex.api_request", "model", "safe", at), field(v, "logs/codex.api_request", "attempt", "safe", at)}
	}
	var cat Catalog
	mergeFields(&cat, both("0.150.0", night(0)), nil)
	for i := 1; i <= keepVersions; i++ {
		v := fmt.Sprintf("0.15%d.0", i)
		mergeFields(&cat, both(v, night(i)), map[string]bool{"codex\x00" + v: true})
	}
	tonight := []e2e.FieldRow{field("0.160.0", "logs/codex.api_request", "model", "safe", night(9))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.160.0"}})
	_, _, hs := fieldDrift(cat, tonight, ran, night(9))
	h := hs[0]
	if len(h.Removed) != 1 || h.Removed[0].Key != "attempt" {
		t.Errorf("removed %+v, want attempt", h.Removed)
	}
	if h.Previous != "0.155.0" || h.Since != "0.150.0" || !strings.Contains(h.headline(), "(was 0.155.0; judged against 0.150.0, the last whole census)") {
		t.Errorf("previous %s, since %s, headline %q", h.Previous, h.Since, h.headline())
	}
}

// A key only a failed run saw, an error path's, is no evidence the build before sent it: a
// whole build without it reports nothing removed.
func TestAKeyOnlyAPartialRunSawIsNotRemoved(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.150.0", "logs/codex.api_request", "model", "safe", day)}, nil)
	mergeFields(&cat, []e2e.FieldRow{
		field("0.151.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.151.0", "logs/codex.api_request", "error.message", "prompt", day.Add(24*time.Hour)),
	}, map[string]bool{"codex\x000.151.0": true})
	tonight := []e2e.FieldRow{field("0.152.0", "logs/codex.api_request", "model", "safe", day.Add(48*time.Hour))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.152.0"}})
	if _, _, hs := fieldDrift(cat, tonight, ran, day.Add(48*time.Hour)); len(hs[0].Removed) != 0 {
		t.Errorf("removed %+v: only a failed run sent it", hs[0].Removed)
	}
}

// Ten partial builds in a row, the first five seeing a key and the rest not, prune neither
// the key nor the whole census that saw it: the whole build after them reports it removed.
func TestTenPartialBuildsKeepAKeyTheWholeCensusSaw(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.150.0", "logs/codex.api_request", "model", "safe", night(0)),
		field("0.150.0", "logs/codex.api_request", "attempt", "safe", night(0))}, nil)
	for i := 1; i <= 2*keepCensuses; i++ {
		v := fmt.Sprintf("0.1%d.0", 50+i)
		rows := []e2e.FieldRow{field(v, "logs/codex.api_request", "model", "safe", night(i))}
		if i <= keepCensuses {
			rows = append(rows, field(v, "logs/codex.api_request", "attempt", "safe", night(i)))
		}
		mergeFields(&cat, rows, map[string]bool{"codex\x00" + v: true})
	}
	if !slices.ContainsFunc(cat.Fields, func(f FieldEntry) bool { return f.Key == "attempt" && slices.Contains(f.Whole, "0.150.0") }) {
		t.Fatalf("attempt, or its whole build, pruned: %+v", cat.Fields)
	}
	tonight := []e2e.FieldRow{field("0.170.0", "logs/codex.api_request", "model", "safe", night(20))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.170.0"}})
	if _, _, hs := fieldDrift(cat, tonight, ran, night(20)); len(hs[0].Removed) != 1 || hs[0].Removed[0].Key != "attempt" {
		t.Errorf("removed %+v, want attempt", hs[0].Removed)
	}
}

// The nightly job censuses each build about three nights running: a build censused partially
// one night and whole another, in either order, takes nothing from the failed night as
// evidence. A key and a surface only the failed run sent are not reported gone from the next.
func TestAWholeBuildTakesNothingFromItsFailedNight(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	night := func(i int) time.Time { return day.Add(time.Duration(i) * 24 * time.Hour) }
	failedRun := []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", night(1)),
		field("0.163.0", "logs/codex.api_request", "error.message", "prompt", night(1)),
		field("0.163.0", "logs/codex.stream_error", "reason", "safe", night(1))}
	wholeRun := []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", night(2))}
	failed := map[string]bool{"codex\x000.163.0": true}
	for _, order := range []string{"failed first", "whole first"} {
		var cat Catalog
		mergeFields(&cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", night(0))}, nil)
		if order == "failed first" {
			mergeFields(&cat, failedRun, failed)
			mergeFields(&cat, wholeRun, nil)
		} else {
			mergeFields(&cat, wholeRun, nil)
			mergeFields(&cat, failedRun, failed)
		}
		tonight := []e2e.FieldRow{field("0.164.0", "logs/codex.api_request", "model", "safe", night(3))}
		ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0"}})
		_, _, hs := fieldDrift(cat, tonight, ran, night(3))
		if h := hs[0]; len(h.Removed)+len(h.Unseen) != 0 {
			t.Errorf("%s: removed %+v, unseen %+v: only the failed run sent them", order, h.Removed, h.Unseen)
		}
	}
}

// A build the catalog has only partially is new evidence when censused whole, though a newer
// build comes the same night: what its whole census first reaches is new.
func TestWhatAWholeCensusFirstReachesIsNew(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", day)}, nil)
	mergeFields(&cat, []e2e.FieldRow{field("0.164.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour))},
		map[string]bool{"codex\x000.164.0": true})
	tonight := []e2e.FieldRow{
		field("0.164.0", "logs/codex.api_request", "model", "safe", day.Add(48*time.Hour)),
		field("0.164.0", "logs/codex.api_request", "reached_whole", "safe", day.Add(48*time.Hour)),
		field("0.165.0", "logs/codex.api_request", "model", "safe", day.Add(48*time.Hour)),
	}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0"}, {Harness: "codex", Version: "0.165.0", Failed: true}})
	_, _, hs := fieldDrift(cat, tonight, ran, day.Add(48*time.Hour))
	if a := hs[0].Added; len(a) != 1 || a[0].Key != "reached_whole" {
		t.Errorf("added %+v, want reached_whole", a)
	}
}

// Seeded histories of nightly runs as the nightly job runs them: each night the newest three
// releases, a release now and then adding and dropping keys, runs that fail partway (seeing
// part of what the build sends, and keys of an error path), builds not reached, and the
// catalog read back from fields.json. Whatever goes into the catalog is said new that night,
// and nothing said gone is sent by the build it was judged gone from.
func TestNightlyHistories(t *testing.T) {
	for seed := range int64(200) {
		rng := rand.New(rand.NewPCG(uint64(seed), 1))
		dir := t.TempDir()
		path := filepath.Join(dir, "fields.json")
		sends := [][]string{{"k0", "k1", "k2"}}
		var cat Catalog
		for n := range 20 {
			at := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC).Add(time.Duration(n) * 24 * time.Hour)
			for range rng.IntN(3) {
				keys := slices.Clone(sends[len(sends)-1])
				if len(keys) > 1 && rng.IntN(2) == 0 {
					i := rng.IntN(len(keys))
					keys = slices.Delete(keys, i, i+1)
				}
				keys = append(keys, fmt.Sprintf("k%d_%d", n, len(sends)))
				sends = append(sends, keys)
			}
			var rows []e2e.FieldRow
			var runs []e2e.CensusRun
			truly := map[string][]string{} // what each build sends, whatever a run saw
			for b := max(0, len(sends)-3); b < len(sends); b++ {
				v := fmt.Sprintf("0.%d.0", 100+b)
				if rng.IntN(10) == 0 {
					continue // not reached
				}
				failed := rng.IntN(4) == 0
				truly[v] = sends[b]
				runs = append(runs, e2e.CensusRun{Harness: "codex", Version: v, Failed: failed})
				for _, k := range sends[b] {
					if !failed || rng.IntN(2) == 0 {
						rows = append(rows, field(v, "logs/codex.api_request", k, "safe", at))
					}
				}
				if failed {
					rows = append(rows, field(v, "logs/codex.api_request", "error.message", "prompt", at))
				}
			}
			ran := censusRan(runs)
			_, _, hs := fieldDrift(cat, rows, ran, at)
			before := map[string]bool{}
			for _, f := range cat.Fields {
				before[f.Key] = true
			}
			for _, h := range hs {
				if h.First {
					continue
				}
				said := map[string]bool{}
				for _, c := range h.Added {
					said[c.Key] = true
				}
				for _, r := range rows {
					if !before[r.Key] && !said[r.Key] {
						t.Fatalf("seed %d night %d: %s went into the catalog unsaid", seed, n, r.Key)
					}
				}
				for _, c := range h.Removed {
					in := c.In
					if in == "" {
						in = h.lastJudged()
					}
					if slices.Contains(truly[in], c.Key) || c.Key == "error.message" {
						t.Fatalf("seed %d night %d: %s said gone in %s, which sent it", seed, n, c.Key, in)
					}
				}
			}
			mergeFields(&cat, rows, ran.failed)
			if err := writeCatalog(path, cat); err != nil {
				t.Fatal(err)
			}
			cat = Catalog{}
			if err := readJSON(path, &cat); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A key one build of a night drops and a later one sends again says so; and a harness the
// catalog has only partially, two builds censused whole tonight, has the newer judged against
// the older.
func TestAChainSaysWhatCameBack(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", day),
		field("0.163.0", "logs/codex.api_request", "flaps", "safe", day)}, nil)
	tonight := []e2e.FieldRow{field("0.164.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.165.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.165.0", "logs/codex.api_request", "flaps", "safe", day.Add(24*time.Hour))}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.164.0"}, {Harness: "codex", Version: "0.165.0"}})
	_, _, hs := fieldDrift(cat, tonight, ran, day.Add(24*time.Hour))
	if md := (Drift{Harnesses: hs}).markdown(); !strings.Contains(md, "`flaps` on `logs/codex.api_request` (safe) (gone in 0.164.0, back in 0.165.0)") {
		t.Errorf("drift.md:\n%s", md)
	}

	var partial Catalog
	mergeFields(&partial, []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", day)}, map[string]bool{"codex\x000.163.0": true})
	two := []e2e.FieldRow{field("0.164.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.164.0", "logs/codex.api_request", "dropped", "safe", day.Add(24*time.Hour)),
		field("0.165.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour))}
	_, _, hs = fieldDrift(partial, two, ran, day.Add(24*time.Hour))
	if h := hs[0]; h.Since != "0.164.0" || len(h.Removed) != 1 || h.Removed[0].Key != "dropped" {
		t.Errorf("since %s, removed %+v", h.Since, h.Removed)
	}
}
