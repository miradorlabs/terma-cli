package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// Publishing: the run's snapshots merged into the published tree at out (docs/models), each
// model's timeline grown by what changed since the run before, its notes made again, and the
// index and feed written.

// maxRecent is how many events recent.json keeps.
const maxRecent = 200

// probeParallel is how many models are probed at once.
const probeParallel = 4

// validID is what a model id must be to name a folder: a letter or digit, then letters, digits
// and . _ -, never "..".
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// published is what the tree at out held before the run.
type published struct {
	snaps     map[string]*Snapshot
	timelines map[string]*Timeline
	notes     map[string]*Notes
	recent    []RecentEvent
	vendors   map[string]bool // vendors with a model published
}

func readPublished(out string) (*published, error) {
	p := &published{snaps: map[string]*Snapshot{}, timelines: map[string]*Timeline{}, notes: map[string]*Notes{}, vendors: map[string]bool{}}
	var idx Index
	if err := readOptional(filepath.Join(out, "index.json"), &idx); err != nil {
		return nil, err
	}
	for _, e := range idx.Models {
		if !validID.MatchString(e.ID) {
			continue
		}
		var s Snapshot
		if err := readOptional(filepath.Join(out, e.ID, "snapshot.json"), &s); err != nil {
			return nil, err
		}
		if s.ID == "" {
			continue
		}
		normalizeFacts(s.Facts)
		for _, vals := range s.Provenance {
			normalizeFacts(vals)
		}
		p.snaps[s.ID] = &s
		p.vendors[s.Vendor] = true
		t, err := readTimelineFiles(filepath.Join(out, e.ID))
		if err != nil {
			return nil, err
		}
		p.timelines[s.ID] = t
		var n Notes
		if err := readOptional(filepath.Join(out, e.ID, "notes.json"), &n); err != nil {
			return nil, err
		}
		p.notes[s.ID] = &n
	}
	var r Recent
	if err := readOptional(filepath.Join(out, "recent.json"), &r); err != nil {
		return nil, err
	}
	p.recent = r.Events
	return p, nil
}

// normalizeFacts gives facts read back from JSON the types a run makes: counts int64, lists of
// words []string, maps of ids map[string]string.
func normalizeFacts(f map[string]any) {
	for k, v := range f {
		switch x := v.(type) {
		case float64:
			f[k] = int64(x)
		case []any:
			ss := make([]string, 0, len(x))
			for _, e := range x {
				ss = append(ss, fmt.Sprint(e))
			}
			f[k] = ss
		case map[string]any:
			m := map[string]string{}
			for a, b := range x {
				m[a] = fmt.Sprint(b)
			}
			f[k] = m
		}
	}
}

// result is a run's published state, for announcements.
type result struct {
	snaps    map[string]*Snapshot
	notes    map[string]*Notes
	new      []string // models first listed this run
	vendored map[string]bool
	// observed are the models this run's probes observed: the plain request answered, or refused
	// as the model's.
	observed []string
}

func publish(out string, r *run, prev *published, uses map[string][]HarnessUse, changelogs map[string][]ChangelogEntry,
	probe func(*Snapshot) *Probes) (*result, error) {
	ids := followed(r, prev.snaps)
	vendorOf := map[string]string{}
	for _, o := range r.obs {
		if o.listed {
			vendorOf[o.id] = o.vendor
		}
	}
	for id, s := range prev.snaps {
		if _, ok := vendorOf[id]; !ok {
			vendorOf[id] = s.Vendor
		}
	}
	snaps := map[string]*Snapshot{}
	for id, listed := range ids {
		if !validID.MatchString(id) {
			continue
		}
		s := assemble(id, vendorOf[id], r, prev.snaps[id], listed)
		s.Harnesses = usesFor(uses, s)
		snaps[id] = s
	}
	var observed []string
	// Probes, where a key is configured and the run asks for them (probe says which), a few
	// models at a time: each model's cases run one after another.
	if probe != nil {
		var todo []*Snapshot
		for _, id := range sortedKeys(snaps) {
			s := snaps[id]
			if st, _ := s.Facts[factStatus].(string); s.Listed && st != "retired" {
				todo = append(todo, s)
			}
		}
		probed := make([]*Probes, len(todo))
		var wg sync.WaitGroup
		sem := make(chan struct{}, probeParallel)
		// A vendor that refused its key once this run (revoked, out of credit) is not asked again
		// for its other models: each would be refused the same.
		var mu sync.Mutex
		refused := map[string]string{}
		for i, s := range todo {
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				mu.Lock()
				_, skip := refused[s.Vendor]
				mu.Unlock()
				if skip {
					return
				}
				p := probe(s)
				if p != nil && p.keyRefused {
					mu.Lock()
					refused[s.Vendor] = strings.Join(p.Errors, "; ")
					mu.Unlock()
				}
				probed[i] = p
			})
		}
		wg.Wait()
		keyWide(todo, probed)
		for _, v := range sortedKeys(refused) {
			// A GitHub workflow command, so the run shows it: the key needs someone.
			fmt.Printf("::warning::%s refused the reference's key, its models not probed this run: %s\n", v, refused[v])
		}
		for i, p := range probed {
			// Observed: the plain request answered, and the key not refused partway (the rest is
			// due again next run).
			if p != nil && !p.unanswered["baseline"] && !p.keyRefused {
				observed = append(observed, todo[i].ID)
			}
		}
		for i, s := range todo {
			p := probed[i]
			if p == nil {
				continue
			}
			if s.API == nil {
				s.API = &API{}
			}
			// What this run did not observe keeps what was observed before, and probes that saw what
			// the ones before saw are kept whole, their time included: a run that only confirms them
			// changes no file.
			p.At = r.now
			for _, e := range p.Errors {
				fmt.Fprintf(os.Stderr, "modelref: %s: not observed: %s\n", s.ID, e)
			}
			p = keepUnobserved(s.API.Probes, p)
			s.API.Probes = p
		}
	}
	tokenizerLike(snaps)
	pred, succ := lineage(snaps)
	names := harnessNames(uses)
	res := &result{snaps: snaps, notes: map[string]*Notes{}, vendored: prev.vendors, observed: observed}
	recent := prev.recent
	var entries []Entry
	for _, id := range sortedKeys(snaps) {
		s := snaps[id]
		tl := prev.timelines[id]
		if tl == nil {
			tl = &Timeline{SchemaVersion: schemaVersion, ID: id, Steps: []Step{}}
		}
		var step *Step
		if old := prev.snaps[id]; old == nil {
			step = &Step{At: r.now, Event: "listed", Changes: []refdiff.Change{}}
			res.new = append(res.new, id)
		} else if changes := compare(view(old), view(s), names); len(changes) > 0 {
			ev := "changed"
			if old.Listed && !s.Listed {
				ev = "unlisted"
			} else if !old.Listed && s.Listed {
				ev = "listed"
			}
			step = &Step{At: r.now, Event: ev, Changes: changes}
		}
		if step != nil {
			tl.Steps = append([]Step{*step}, tl.Steps...)
			// A step that only follows from changes elsewhere (a site-wide edit of the vendor's
			// pages, a new model that counts tokens the same) touches many models at once: it stays
			// on each model's timeline, out of the feed.
			if !incidental(step.Changes) {
				recent = append([]RecentEvent{{At: r.now, Model: id, Vendor: s.Vendor, Name: s.Name, Event: step.Event,
					Changes: len(step.Changes), Highlights: highlights(step.Changes, 3)}}, recent...)
			}
		}
		// A vendor's changelog not read this run (an outage, or a run of other vendors) keeps the
		// entries the notes had of it.
		var named []ChangelogEntry
		if log, ok := changelogs[s.Vendor]; ok {
			named = mentions(log, s.ID, s.Name)
		} else if old := prev.notes[id]; old != nil && old.Changelog != nil {
			named = old.Changelog
		} else {
			named = []ChangelogEntry{}
		}
		n := notes(s, snaps[pred[id]], named, names)
		res.notes[id] = n
		dir := filepath.Join(out, id)
		for name, v := range map[string]any{"snapshot.json": s, "notes.json": n} {
			if err := writeJSON(filepath.Join(dir, name), v); err != nil {
				return nil, err
			}
		}
		if err := writeTimelineFiles(dir, *tl, timelineSteps); err != nil {
			return nil, err
		}
		entries = append(entries, entry(s, tl, pred[id], succ[id]))
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := deref(entries[i].ReleasedAt), deref(entries[j].ReleasedAt)
		if ri != rj {
			return ri > rj
		}
		return entries[i].ID < entries[j].ID
	})
	var vs []VendorInfo
	for _, v := range vendors {
		n := 0
		for _, e := range entries {
			if e.Vendor == v.id {
				n++
			}
		}
		vs = append(vs, VendorInfo{ID: v.id, Name: v.name, Docs: v.docsURL, Changelog: v.changelogURL, Models: n})
	}
	if len(recent) > maxRecent {
		recent = recent[:maxRecent]
	}
	// As of the newest change on any timeline (the feed leaves some out): a run that changed
	// nothing writes the same bytes.
	asOf := time.Time{}
	for _, e := range entries {
		if e.LastChange != nil && e.LastChange.At.After(asOf) {
			asOf = e.LastChange.At
		}
	}
	if len(recent) > 0 && recent[0].At.After(asOf) {
		asOf = recent[0].At
	}
	if err := writeJSON(filepath.Join(out, "index.json"), Index{SchemaVersion: schemaVersion, GeneratedAt: asOf, Vendors: vs, Models: entries}); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(out, "recent.json"), Recent{SchemaVersion: schemaVersion, GeneratedAt: asOf, Events: recent}); err != nil {
		return nil, err
	}
	return res, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// keyWide drops the probes of a vendor whose every probed model, two or more, was refused as
// unavailable in one run: that is the key's doing (its access changed, the vendor's error for a
// key problem), not every model at once, and what the probes saw before stands.
func keyWide(todo []*Snapshot, probed []*Probes) (dropped int) {
	type count struct{ probed, unavailable int }
	by := map[string]*count{}
	for i, s := range todo {
		if probed[i] == nil {
			continue
		}
		c := by[s.Vendor]
		if c == nil {
			c = &count{}
			by[s.Vendor] = c
		}
		c.probed++
		if probed[i].Unavailable != "" {
			c.unavailable++
		}
	}
	for i, s := range todo {
		if c := by[s.Vendor]; probed[i] != nil && c.probed >= 2 && c.unavailable == c.probed {
			fmt.Fprintf(os.Stderr, "modelref: %s: every model refused (%s): the key's doing; probes kept\n", s.Vendor, probed[i].Unavailable)
			probed[i] = nil
			dropped++
		}
	}
	return dropped
}

// incidental says every change is to the text of the vendor's pages, or to which other models
// count tokens the same.
func incidental(changes []refdiff.Change) bool {
	for _, c := range changes {
		if c.Category != "docs" && !(len(c.Path) >= 2 && c.Path[0] == "api" && c.Path[1] == "tokenizer_like") {
			return false
		}
	}
	return len(changes) > 0
}

// timelineSteps is how many steps timeline.json holds, and each archive at most: the newest
// steps, which a model's page reads, in one bounded file, and every older one in a file of its own.
const timelineSteps = 25

// readTimelineFiles is a model's whole timeline: timeline.json's steps, then its archives'.
func readTimelineFiles(dir string) (*Timeline, error) {
	var t Timeline
	if err := readOptional(filepath.Join(dir, "timeline.json"), &t); err != nil {
		return nil, err
	}
	// Archives count from the oldest: the newest archive's steps follow timeline.json's.
	for i := len(t.Archives) - 1; i >= 0; i-- {
		var a Timeline
		if err := readOptional(filepath.Join(dir, t.Archives[i].File), &a); err != nil {
			return nil, err
		}
		t.Steps = append(t.Steps, a.Steps...)
	}
	t.Archives = nil
	return &t, nil
}

// writeTimelineFiles writes t's newest steps (at most per) to timeline.json, and the older ones
// to timeline-1.json, timeline-2.json and on, per steps each counted from the oldest, so a full
// archive's steps never move; an archive no longer needed is removed.
func writeTimelineFiles(dir string, t Timeline, per int) error {
	head, older := t, []Step(nil)
	if len(t.Steps) > per {
		head.Steps, older = t.Steps[:per], slices.Clone(t.Steps[per:])
	}
	head.Archives = nil
	slices.Reverse(older) // oldest first
	n := 0
	for len(older) > 0 {
		n++
		chunk := older[:min(per, len(older))]
		older = older[len(chunk):]
		steps := slices.Clone(chunk)
		slices.Reverse(steps) // newest first, as every timeline lists them
		name := fmt.Sprintf("timeline-%d.json", n)
		head.Archives = append(head.Archives, TimelineArchive{File: name, First: chunk[0].At, Last: chunk[len(chunk)-1].At, Steps: len(chunk)})
		if err := writeJSON(filepath.Join(dir, name), Timeline{SchemaVersion: t.SchemaVersion, ID: t.ID, Steps: steps}); err != nil {
			return err
		}
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "timeline-*.json"))
	for _, f := range stale {
		var k int
		if _, err := fmt.Sscanf(filepath.Base(f), "timeline-%d.json", &k); err == nil && k > n {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	return writeJSON(filepath.Join(dir, "timeline.json"), head)
}

// notes are a model's release notes: what its vendor says is new, its changelog's entries that
// name it (named), and what differs from the model before it.
func notes(s, before *Snapshot, named []ChangelogEntry, names map[string]string) *Notes {
	n := &Notes{SchemaVersion: schemaVersion, ID: s.ID, Changelog: named,
		Changes: []refdiff.Change{}, Highlights: []string{}}
	if p, ok := s.Docs["whats_new"]; ok {
		n.WhatsNew = &p
	}
	n.VendorHighlights = vendorHighlights(n.WhatsNew, launch(n.Changelog), 5)
	if before == nil {
		return n
	}
	n.Predecessor = &before.ID
	for _, c := range compare(noteView(before), noteView(s), names) {
		// A fact one of the two has no source for is unknown there, not new or gone; and a
		// refusal's wording differing between two models is no news of either.
		if c.Category == "facts" && len(c.Path) == 2 && c.Op != "changed" {
			continue
		}
		if c.Title = noteTitle(c, before); c.Title != "" {
			n.Changes = append(n.Changes, c)
		}
	}
	n.Highlights = highlights(n.Changes, 5)
	if pct, ok := tokenRatio(before, s); ok && (pct >= 1 || pct <= -1) {
		more := "more"
		if pct < 0 {
			more, pct = "fewer", -pct
		}
		n.Highlights = append([]string{fmt.Sprintf("Counts %.0f%% %s tokens than %s for the same text", pct, more, before.Name)}, n.Highlights...)
	}
	return n
}

// launch are the entries of a model's changelog mentions under the oldest date: the ones that
// announced it.
func launch(entries []ChangelogEntry) []ChangelogEntry {
	oldest := ""
	for _, e := range entries {
		if oldest == "" || e.Date < oldest {
			oldest = e.Date
		}
	}
	var out []ChangelogEntry
	for _, e := range entries {
		if e.Date == oldest {
			out = append(out, e)
		}
	}
	return out
}

// noteTitle says a difference from the model before as news of this one.
func noteTitle(c refdiff.Change, before *Snapshot) string {
	p := c.Path
	if c.Category == "api" && len(p) == 5 && p[2] == "params" && p[4] == "accepted" {
		if c.After == true {
			return "Accepts " + p[3] + " (" + before.Name + " rejects it)"
		}
		return "Rejects " + p[3] + " (" + before.Name + " accepts it)"
	}
	if c.Category == "api" && len(p) >= 4 && p[2] == "params" {
		return ""
	}
	return title(c, nil)
}

// highlights are the n changes most worth a headline: the ones that break a caller first, then
// facts, then the API, then the rest; text changes of pages last.
func highlights(changes []refdiff.Change, n int) []string {
	rank := func(c refdiff.Change) int {
		r := map[string]int{"breaking": 0, "notable": 10, "informational": 20}[c.Impact]
		r += map[string]int{"listed": 0, "facts": 1, "api": 2, "harnesses": 3, "docs": 4}[c.Category]
		return r
	}
	sorted := slices.Clone(changes)
	sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i]) < rank(sorted[j]) })
	out := []string{}
	for _, c := range sorted {
		if len(out) == n {
			break
		}
		if c.Title != "" && !slices.Contains(out, c.Title) {
			out = append(out, c.Title)
		}
	}
	return out
}

func entry(s *Snapshot, tl *Timeline, pred, succ string) Entry {
	e := Entry{ID: s.ID, Vendor: s.Vendor, Name: s.Name, Family: s.Family, FirstSeen: s.FirstSeen, Harnesses: []string{},
		Probed: s.API != nil && s.API.Probes != nil && s.API.Probes.Unavailable == ""}
	if v, ok := s.Facts[factStatus].(string); ok {
		e.Status = &v
	}
	if !s.Listed {
		v := "unlisted"
		e.Status = &v
	}
	if v, ok := s.Facts[factReleasedAt].(string); ok {
		e.ReleasedAt = &v
	}
	if v, ok := s.Facts[factContextWindow].(int64); ok {
		e.ContextWindow = &v
	}
	if v, ok := s.Facts[factMaxOutput].(int64); ok {
		e.MaxOutput = &v
	}
	if v, ok := s.Facts[factKnowledgeCutoff].(string); ok {
		e.KnowledgeCutoff = &v
	}
	if v, ok := s.Facts[factReasoning].(bool); ok {
		e.Reasoning = &v
	}
	if pred != "" {
		e.Predecessor = &pred
	}
	if succ != "" {
		e.Successor = &succ
	}
	for _, u := range s.Harnesses {
		if u.Current && !slices.Contains(e.Harnesses, u.Harness) {
			e.Harnesses = append(e.Harnesses, u.Harness)
		}
	}
	for _, st := range tl.Steps {
		if st.Event != "listed" || len(st.Changes) > 0 {
			e.LastChange = &LastChange{At: st.At, Changes: len(st.Changes)}
			break
		}
	}
	return e
}

func harnessNames(uses map[string][]HarnessUse) map[string]string {
	names := map[string]string{}
	for _, list := range uses {
		for _, u := range list {
			names[u.Harness] = u.Name
		}
	}
	return names
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// writeJSON writes v, indented, with a trailing newline, creating its folder; HTML is not
// escaped, so the published text reads as the vendor wrote it.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func readOptional(path string, v any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
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
