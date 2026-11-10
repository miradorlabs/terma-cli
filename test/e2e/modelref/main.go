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
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	out := flag.String("out", "docs/models", "the published tree to merge into")
	harnesses := flag.String("harnesses", "", "the Harness Reference's published tree (docs/harnesses), for which harnesses use each model")
	probeMode := flag.String("probe", "new", "which models to probe where a vendor's key is set: new (never probed), all, or none")
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
	case "new", "all", "none":
	default:
		fail(fmt.Errorf("-probe %q: want new, all or none", *probeMode))
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
	uses := map[string][]HarnessUse{}
	if *harnesses != "" {
		if uses, err = harnessUses(*harnesses); err != nil {
			fail(fmt.Errorf("-harnesses: %w", err))
		}
	}
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
	var probe func(*Snapshot) *Probes
	if *probeMode != "none" {
		probe = func(s *Snapshot) *Probes {
			v := vendorByID(s.Vendor)
			key := os.Getenv(v.keyEnv)
			if key == "" || (len(wanted) > 0 && !wanted[v.id]) {
				return nil
			}
			if *probeMode == "new" && s.API != nil && s.API.Probes != nil {
				return nil
			}
			return v.probe(ctx, c, key, s)
		}
	}
	res, err := publish(*out, r, prev, uses, changelogs, probe)
	if err != nil {
		fail(err)
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
	r := &run{now: now, read: map[string]bool{}, failed: map[string]string{}, wanted: wanted}
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

func reflectEqual(a, b any) bool { return reflect.DeepEqual(roundTrip(a), roundTrip(b)) }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "modelref:", err)
	os.Exit(1)
}
