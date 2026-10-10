// Command compatgen keeps terma's compatibility matrix and field catalog: it merges the live
// suite's runs (report/compat.json, one per run and platform) into the history
// (docs/compat/history.json), and renders the matrix people read
// (docs/COMPATIBILITY.md) and the data the website reads (docs/compat/compat.json); and it
// merges each run's field census (report/fields.json) into the field catalog
// (docs/compat/fields.json), rendered as docs/FIELDS.md.
//
//	go run ./compatgen -run report/compat.json [-fields report/fields.json] [-digest report [-source]]
//
// With -digest it first writes what the runs changed against the history and catalog as they
// were: report/drift.json, drift.md, and slack.json for a Slack incoming webhook. With -source
// the digest links each finding to the lines of a public harness source that name it
// (source.go).
//
// The history keeps, per harness build, platform and capability, the latest result
// with when it ran and when it first passed; a build that stops being tested keeps its
// last result, dated. Nothing here runs a harness.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

type runsFlag []string

func (r *runsFlag) String() string     { return strings.Join(*r, ",") }
func (r *runsFlag) Set(v string) error { *r = append(*r, v); return nil }

// Entry is one harness build × platform × capability in the history.
type Entry struct {
	Harness     string    `json:"harness"`
	Version     string    `json:"version"`
	Platform    string    `json:"platform"`
	Capability  string    `json:"capability"`
	Result      string    `json:"result"` // pass | fail | not run
	LastRun     time.Time `json:"last_run"`
	FirstPassed time.Time `json:"first_passed,omitzero"`
	Terma       string    `json:"terma,omitempty"`
	Test        string    `json:"test,omitempty"`
}

func (e Entry) key() string {
	return e.Harness + "|" + e.Version + "|" + e.Platform + "|" + e.Capability
}

// config is what one compatgen run reads and writes.
type config struct {
	runs, fieldRuns   []string
	history, md, json string
	catalog, fieldsMD string
	digest, link      string
	// source links the digest's findings to the source of harnesses whose source is public.
	source bool
}

func main() {
	var c config
	var runs, fields runsFlag
	flag.Var(&runs, "run", "a run's compat.json (repeatable)")
	flag.Var(&fields, "fields", "a run's fields.json (repeatable)")
	flag.StringVar(&c.history, "history", "../../docs/compat/history.json", "the history, read and rewritten")
	flag.StringVar(&c.md, "md", "../../docs/COMPATIBILITY.md", "the rendered matrix")
	flag.StringVar(&c.json, "json", "../../docs/compat/compat.json", "the matrix for the website")
	flag.StringVar(&c.catalog, "catalog", "../../docs/compat/fields.json", "the field catalog, read and rewritten")
	flag.StringVar(&c.fieldsMD, "fields-md", "../../docs/FIELDS.md", "the rendered field catalog")
	flag.StringVar(&c.digest, "digest", "", "write what the runs changed (drift.json, drift.md, slack.json) to this directory")
	flag.StringVar(&c.link, "link", "", "the run, linked from the digest")
	flag.BoolVar(&c.source, "source", false, "link the digest's findings to the lines of a public harness source that name them (downloads it)")
	flag.Parse()
	c.runs, c.fieldRuns = runs, fields
	if err := run(c, time.Now().UTC()); err != nil {
		fmt.Fprintln(os.Stderr, "compatgen:", err)
		os.Exit(1)
	}
}

func run(c config, now time.Time) error {
	hist := map[string]Entry{}
	var prior []Entry
	if err := readJSON(c.history, &prior); err != nil {
		return err
	}
	for _, e := range prior {
		hist[e.key()] = e
	}
	var catalog Catalog
	if err := readJSON(c.catalog, &catalog); err != nil {
		return err
	}
	var compatRows []e2e.CompatRow
	for _, p := range c.runs {
		var rows []e2e.CompatRow
		if err := readJSON(p, &rows); err != nil {
			return err
		}
		compatRows = append(compatRows, rows...)
	}
	var fieldRows []e2e.FieldRow
	for _, p := range c.fieldRuns {
		var rows []e2e.FieldRow
		if err := readJSON(p, &rows); err != nil {
			return err
		}
		fieldRows = append(fieldRows, rows...)
	}
	if c.digest != "" {
		d := Drift{GeneratedAt: now, Link: c.link, Compat: compatDrift(hist, compatRows)}
		d.NoCensus, d.Missing, d.Harnesses = fieldDrift(catalog, fieldRows, censusRan(compatRows), now)
		if c.source {
			linkSources(&d)
		}
		if err := writeDigest(c.digest, d); err != nil {
			return err
		}
	}
	for _, r := range compatRows {
		merge(hist, Entry{Harness: r.Harness, Version: r.Version, Platform: r.Platform, Capability: r.Capability,
			Result: r.Result, LastRun: r.At, Terma: r.Terma, Test: r.Test})
	}
	if len(fieldRows) > 0 || len(catalog.Fields) > 0 {
		mergeFields(&catalog, fieldRows)
		catalog.GeneratedAt = now
		if err := writeCatalog(c.catalog, catalog); err != nil {
			return err
		}
		if err := writeFile(c.fieldsMD, []byte(renderFields(catalog, now))); err != nil {
			return err
		}
	}
	entries := make([]Entry, 0, len(hist))
	for _, e := range hist {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
	if err := writeJSON(c.history, entries); err != nil {
		return err
	}
	if err := writeJSON(c.json, website(entries, now)); err != nil {
		return err
	}
	return writeFile(c.md, []byte(render(entries, now)))
}

// writeDigest writes d as drift.json, drift.md and slack.json in dir.
func writeDigest(dir string, d Drift) error {
	if err := writeJSON(filepath.Join(dir, "drift.json"), d); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "drift.md"), []byte(d.markdown())); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "slack.json"), d.slack())
}

// merge files e in the history: a newer result replaces an older one; the first pass
// is remembered; a run that did not get to a capability keeps what was known of it.
func merge(hist map[string]Entry, e Entry) {
	prev, ok := hist[e.key()]
	if ok && prev.LastRun.After(e.LastRun) {
		return
	}
	if ok && e.Result == "not run" && prev.Result != "not run" {
		return
	}
	if ok {
		e.FirstPassed = prev.FirstPassed
	}
	if e.FirstPassed.IsZero() && e.Result == "pass" {
		e.FirstPassed = e.LastRun
	}
	hist[e.key()] = e
}

// --- rendering -----------------------------------------------------------------------

var icon = map[string]string{"pass": "✅", "fail": "❌", "not run": "⚪"}

// platformShort names a platform in a cell.
func platformShort(p string) string {
	switch {
	case strings.HasPrefix(p, "darwin"):
		return "macOS"
	case strings.HasPrefix(p, "linux"):
		return "Linux"
	case strings.HasPrefix(p, "windows"):
		return "Windows"
	}
	return p
}

// maxVersions is how many of a harness's builds the matrix shows, newest first.
const maxVersions = 4

func render(entries []Entry, now time.Time) string {
	var b strings.Builder
	b.WriteString("# Harness compatibility\n\n")
	b.WriteString("Generated by `make compat` (test/e2e/compatgen) from the live suite's runs against the real harness binaries,")
	b.WriteString(" with fake model providers and a fake Terma, so no run needs an account or spends anything.")
	fmt.Fprintf(&b, " Last generated %s. Do not edit by hand: the history is `docs/compat/history.json`.\n\n", now.Format("2006-01-02"))
	b.WriteString("✅ passed · ❌ failed · ⚪ not run in the latest run that covered it. Each cell names the platforms, and the date of the latest run.\n\n")

	byHarness := map[string][]Entry{}
	for _, e := range entries {
		byHarness[e.Harness] = append(byHarness[e.Harness], e)
	}
	b.WriteString("## At a glance\n\n| Harness | Newest build verified | Capabilities passing | Last run |\n|---|---|---|---|\n")
	for _, h := range e2e.Harnesses {
		es := byHarness[h.ID]
		if len(es) == 0 {
			fmt.Fprintf(&b, "| %s | — | not yet covered | — |\n", h.Name)
			continue
		}
		versions := versionsOf(es)
		newest := versions[0]
		pass, tested := 0, 0
		var last time.Time
		for _, c := range e2e.Capabilities {
			r, _, ok := cell(es, newest, c.ID)
			if !ok {
				continue
			}
			tested++
			if r == "pass" {
				pass++
			}
		}
		for _, e := range es {
			if e.LastRun.After(last) {
				last = e.LastRun
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %d of %d | %s |\n", h.Name, newest, pass, tested, last.Format("2006-01-02"))
	}
	for _, h := range e2e.Harnesses {
		es := byHarness[h.ID]
		if len(es) == 0 {
			continue
		}
		versions := versionsOf(es)
		if len(versions) > maxVersions {
			versions = versions[:maxVersions]
		}
		fmt.Fprintf(&b, "\n## %s\n\n| Capability |", h.Name)
		for _, v := range versions {
			fmt.Fprintf(&b, " %s |", v)
		}
		b.WriteString("\n|---|")
		for range versions {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		for _, c := range e2e.Capabilities {
			var row strings.Builder
			row.WriteString("| " + c.Label + " |")
			any := false
			for _, v := range versions {
				r, where, ok := cell(es, v, c.ID)
				if !ok {
					row.WriteString(" |")
					continue
				}
				any = true
				row.WriteString(" " + icon[r] + " " + where + " |")
			}
			if any {
				b.WriteString(row.String() + "\n")
			}
		}
	}
	return b.String()
}

// cell is a harness build's result for a capability across platforms: the worst of
// them, and where and when each ran.
func cell(es []Entry, version, capability string) (result, where string, ok bool) {
	rank := map[string]int{"not run": 0, "pass": 1, "fail": 2}
	var parts []string
	var last time.Time
	for _, e := range es {
		if e.Version != version || e.Capability != capability {
			continue
		}
		if !ok || rank[e.Result] > rank[result] || result == "not run" {
			result = e.Result
		}
		ok = true
		if p := platformShort(e.Platform); !contains(parts, p) {
			parts = append(parts, p)
		}
		if e.LastRun.After(last) {
			last = e.LastRun
		}
	}
	sort.Strings(parts)
	return result, strings.Join(parts, ", ") + " · " + last.Format("2006-01-02"), ok
}

// versionsOf lists a harness's builds, newest first.
func versionsOf(es []Entry) []string {
	var vs []string
	for _, e := range es {
		if !contains(vs, e.Version) {
			vs = append(vs, e.Version)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[j], vs[i]) })
	return vs
}

// versionLess orders dotted versions numerically, a pre-release before its release.
func versionLess(a, b string) bool {
	pa, pre1, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	pb, pre2, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")
	fa, fb := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < max(len(fa), len(fb)); i++ {
		var x, y int
		if i < len(fa) {
			x, _ = strconv.Atoi(fa[i])
		}
		if i < len(fb) {
			y, _ = strconv.Atoi(fb[i])
		}
		if x != y {
			return x < y
		}
	}
	switch {
	case pre1 == pre2:
		return false
	case pre1 == "":
		return false
	case pre2 == "":
		return true
	}
	return pre1 < pre2
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// website is the matrix as the website reads it: the capabilities and harnesses, named,
// and every result.
func website(entries []Entry, now time.Time) any {
	return map[string]any{
		"generated_at": now.Format(time.RFC3339),
		"capabilities": e2e.Capabilities,
		"harnesses":    e2e.Harnesses,
		"results":      entries,
	}
}

// --- files ---------------------------------------------------------------------------

func readJSON(path string, v any) error {
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

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
