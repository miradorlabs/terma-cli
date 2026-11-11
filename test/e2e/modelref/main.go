// Command modelref publishes the Model Reference: what each AI model a coding agent runs on is
// and does, as its vendor states it and as its API behaves, which harnesses use it, and what
// changed (modelref/SCHEMA.md). Each run reads every vendor's docs and the public catalogs,
// each vendor's API where its key is set, and probes the models it is asked to, then merges
// what it read into the published tree:
//
//	go run ./modelref -out ../../docs/models -harnesses ../../docs/harnesses -probe new \
//	    -announce "$RUNNER_TEMP/models-announcements.json"
//
// Keys come from ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY and XAI_API_KEY; a vendor
// without one is read from its docs alone, and not probed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	out := flag.String("out", "docs/models", "the published tree to merge into")
	harnesses := flag.String("harnesses", "", "where the Harness Reference is published (a URL, …/docs/harnesses, or a folder), for which harnesses use each model; only builds not yet read are fetched")
	probeMode := flag.String("probe", "new", "which models to probe where a vendor's key is set: new (never probed), all, none, or daily (all once a day, by the time the last full probe was published, and new otherwise)")
	only := flag.String("vendors", "", "read only these vendors (comma-separated ids); the others keep what was published")
	site := flag.String("site", "https://terma.ai", "the reference's public site, for the links announcements carry")
	announcePath := flag.String("announce", "", "write the models to announce (new this run) to this JSON file")
	since := flag.String("announce-since", "", "also announce a model released on or after this date (YYYY-MM-DD) when its vendor is published for the first time")
	nowFlag := flag.String("now", "", "the time the run takes for its own, RFC 3339 (default the current time)")
	flag.Parse()

	now := time.Now().UTC()
	if *nowFlag != "" {
		at, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			fail(fmt.Errorf("-now: %w", err))
		}
		now = at.UTC()
	}
	switch *probeMode {
	case "new", "all", "none", "daily":
	default:
		fail(fmt.Errorf("-probe %q: want new, all, none or daily", *probeMode))
	}
	ctx := context.Background()
	c := newClient()
	prev, err := readPublished(*out)
	if err != nil {
		fail(err)
	}
	wanted := map[string]bool{}
	for v := range strings.SplitSeq(*only, ",") {
		if v = strings.TrimSpace(v); v != "" {
			if vendorByID(v) == nil {
				fail(fmt.Errorf("-vendors: no vendor %q", v))
			}
			wanted[v] = true
		}
	}
	r := read(ctx, c, wanted, now)
	var builds harnessBuilds
	if err := readOptional(filepath.Join(*out, harnessBuildsFile), &builds); err != nil {
		fail(err)
	}
	if *harnesses != "" {
		updated, errs := readHarnessBuilds(ctx, harnessesAt(c, *harnesses), &builds)
		for _, err := range errs {
			r.failed["harnesses"] = err.Error()
		}
		builds = *updated
		if err := writeJSON(filepath.Join(*out, harnessBuildsFile), builds); err != nil {
			fail(err)
		}
	}
	uses := harnessUses(&builds)
	changelogs := map[string][]ChangelogEntry{}
	for _, v := range vendors {
		if len(wanted) > 0 && !wanted[v.id] {
			continue
		}
		entries, err := changelogEntries(ctx, c, v, now)
		if err != nil {
			r.failed[v.id+"-changelog"] = err.Error()
			continue
		}
		changelogs[v.id] = entries
	}
	var state probeState
	if err := readOptional(filepath.Join(*out, probeStateFile), &state); err != nil {
		fail(err)
	}
	mode := effectiveMode(*probeMode, state, now)
	var probe func(*Snapshot) *Probes
	if mode != "none" {
		probe = func(s *Snapshot) *Probes {
			v := vendorByID(s.Vendor)
			key := os.Getenv(v.keyEnv)
			if key == "" || (len(wanted) > 0 && !wanted[v.id]) {
				return nil
			}
			if !needsProbe(s, mode) {
				return nil
			}
			return v.probe(ctx, c, key, s)
		}
	}
	res, err := publish(*out, r, prev, uses, changelogs, probe)
	if err != nil {
		fail(err)
	}
	if mode == "all" && *probeMode != "all" {
		// The daily full probe, done: the next is due a day from now.
		if err := writeJSON(filepath.Join(*out, probeStateFile), probeState{SchemaVersion: schemaVersion, AllAt: now}); err != nil {
			fail(err)
		}
	}
	report(r, res)
	if *announcePath != "" {
		if err := writeJSON(*announcePath, announcements(res, strings.TrimSuffix(*site, "/"), *since)); err != nil {
			fail(err)
		}
	}
}

// read reads every source of every vendor wanted (all, where none is named), and the public
// catalogs, at once.
func read(ctx context.Context, c *client, wanted map[string]bool, now time.Time) *run {
	r := &run{now: now, read: map[string]bool{}, failed: map[string]string{}, wanted: wanted, keyed: map[string]bool{}}
	for _, v := range vendors {
		r.keyed[v.id] = os.Getenv(v.keyEnv) != ""
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	got := func(source string, obs []observation, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			r.failed[source] = err.Error()
			return
		}
		r.read[source] = true
		for _, o := range obs {
			if len(wanted) == 0 || wanted[o.vendor] {
				r.obs = append(r.obs, o)
			}
		}
	}
	for _, v := range vendors {
		if len(wanted) > 0 && !wanted[v.id] {
			continue
		}
		wg.Go(func() {
			obs, err := v.docs(ctx, c, now)
			got(v.id+"-docs", obs, err)
		})
		if key := os.Getenv(v.keyEnv); key != "" {
			wg.Go(func() {
				obs, err := v.api(ctx, c, key)
				got(v.id+"-api", obs, err)
			})
		}
	}
	for _, cat := range catalogs {
		wg.Go(func() {
			obs, err := cat.read(ctx, c)
			got(cat.name, obs, err)
		})
	}
	wg.Wait()
	sort.SliceStable(r.obs, func(i, j int) bool { return r.obs[i].source < r.obs[j].source })
	return r
}

// report says on standard error what the run read and what it published.
func report(r *run, res *result) {
	var read []string
	for src := range r.read {
		read = append(read, src)
	}
	sort.Strings(read)
	fmt.Fprintf(os.Stderr, "modelref: read %s\n", strings.Join(read, ", "))
	for src, err := range r.failed {
		fmt.Fprintf(os.Stderr, "modelref: not read %s: %s\n", src, err)
	}
	fmt.Fprintf(os.Stderr, "modelref: %d models, %d new\n", len(res.snaps), len(res.new))
}

// probeStateFile keeps when every model was last probed, beside the models.
const probeStateFile = "probes.json"

// probeState is probes.json.
type probeState struct {
	SchemaVersion int       `json:"schema_version"`
	AllAt         time.Time `json:"all_at"`
}

// effectiveMode is the probe mode a run takes: daily is all where the last full probe is a day
// old or more (or never was), and new otherwise. A full probe due is never lost to a run that
// did not start: the next one takes it.
func effectiveMode(mode string, st probeState, now time.Time) string {
	if mode != "daily" {
		return mode
	}
	if st.AllAt.IsZero() || now.Sub(st.AllAt) >= 24*time.Hour {
		return "all"
	}
	return "new"
}

// needsProbe says whether a run in mode probes s: every model in "all", none in "none", and in
// "new" a model whose probes never saw anything (no probes, or every case unanswered); one whose
// probes saw an answer, or the plain request refused, is not new.
func needsProbe(s *Snapshot, mode string) bool {
	switch mode {
	case "all":
		return true
	case "new":
		p := (*Probes)(nil)
		if s.API != nil {
			p = s.API.Probes
		}
		return p == nil || len(p.Params) == 0 && p.Unavailable == ""
	}
	return false
}

func reflectEqual(a, b any) bool { return reflect.DeepEqual(roundTrip(a), roundTrip(b)) }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "modelref:", err)
	os.Exit(1)
}
