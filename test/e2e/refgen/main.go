// Command refgen publishes the Harness Reference: what each coding-agent harness build does,
// observed by running it, and what changed from one build to the next (reference/SCHEMA.md).
// It reads a night's evidence (the profiles each build's profile scenarios took, its field
// census, the capability history) and merges it into the published tree:
//
//	go run ./refgen -profiles report/profiles.json -fields report/fields.json \
//	    -census report/census.json -history ../../docs/compat/history.json \
//	    -compat ../../docs/compat/compat.json -out ../../docs/harnesses -fetch
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	var profiles, fields, census, attempted listFlag
	flag.Var(&profiles, "profiles", "a run's report/profiles.json (repeatable: one a platform)")
	flag.Var(&fields, "fields", "a run's report/fields.json, its field census (repeatable)")
	flag.Var(&census, "census", "a run's report/census.json (repeatable)")
	catalog := flag.String("catalog", "", "the field catalog, docs/compat/fields.json: a census for each build it saw whole (a backfill's)")
	flag.Var(&attempted, "attempted", "harness=version,… a run set out to capture (repeatable): published even where it captured nothing")
	history := flag.String("history", "", "the capability history, docs/compat/history.json")
	compat := flag.String("compat", "", "the matrix, docs/compat/compat.json, for capability labels")
	out := flag.String("out", "docs/harnesses", "the published tree to merge into")
	fetch := flag.Bool("fetch", false, "read release dates and changelogs from the web")
	source := flag.Bool("source", false, "link changes to the lines of each public harness's source that name them (a tarball a build)")
	flag.Parse()

	n := night{now: time.Now().UTC(), labels: map[string]string{}}
	for _, f := range profiles {
		var ps []e2e.Profile
		if err := readOptional(f, &ps); err != nil {
			fail(err)
		}
		n.profiles = append(n.profiles, ps...)
	}
	for _, f := range fields {
		var rows []e2e.FieldRow
		if err := readOptional(f, &rows); err != nil {
			fail(err)
		}
		n.fields = append(n.fields, rows...)
	}
	for _, f := range census {
		var runs []CensusRun
		if err := readOptional(f, &runs); err != nil {
			fail(err)
		}
		n.census = append(n.census, runs...)
	}
	for _, a := range attempted {
		harness, versions, ok := strings.Cut(a, "=")
		if !ok {
			fail(fmt.Errorf("-attempted %q: want harness=version,…", a))
		}
		for v := range strings.SplitSeq(versions, ",") {
			if v = strings.TrimSpace(v); v != "" {
				n.attempted = append(n.attempted, buildID{harness, v})
			}
		}
	}
	if *catalog != "" {
		rows, err := catalogRows(*catalog)
		if err != nil {
			fail(err)
		}
		n.fields = append(n.fields, rows...)
	}
	if *history != "" {
		if err := readOptional(*history, &n.history); err != nil {
			fail(err)
		}
	}
	if *compat != "" {
		var m struct {
			Capabilities []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"capabilities"`
		}
		if err := readOptional(*compat, &m); err != nil {
			fail(err)
		}
		for _, c := range m.Capabilities {
			n.labels[c.ID] = c.Label
		}
	}
	opt := options{fetch: *fetch}
	links := newLinker(*source)
	if links != nil {
		opt.linker = links
	}
	err := publish(*out, n, opt)
	links.close()
	if err != nil {
		fail(err)
	}
}

// readOptional reads a JSON file into v; a file that is not there is an empty night of it.
func readOptional(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// catalogRows are the census rows the field catalog holds evidence of: each key, for each
// build a whole census saw it in. A build no whole census saw yields none.
func catalogRows(path string) ([]e2e.FieldRow, error) {
	var cat struct {
		Fields []struct {
			Harness string   `json:"harness"`
			Surface string   `json:"surface"`
			Key     string   `json:"key"`
			Class   string   `json:"class"`
			Kinds   []string `json:"kinds"`
			Whole   []string `json:"whole"`
		} `json:"fields"`
	}
	if err := readOptional(path, &cat); err != nil {
		return nil, err
	}
	var rows []e2e.FieldRow
	for _, f := range cat.Fields {
		for _, v := range f.Whole {
			rows = append(rows, e2e.FieldRow{Harness: f.Harness, Version: v, Surface: f.Surface, Key: f.Key, Kinds: f.Kinds, Class: f.Class})
		}
	}
	return rows, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "refgen:", err)
	os.Exit(1)
}
