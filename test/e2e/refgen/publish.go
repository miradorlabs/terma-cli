package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/refdiff"
	"github.com/miradorlabs/terma-cli/e2e/sourcelink"
)

// Publishing: the night's snapshots merged into the published ones, each harness's versions
// compared in order, and the whole written back. A diff whose two snapshots the night did not
// change is kept as it was (with what its changelog and source said then); one the night did
// is made again. A diff no longer between neighbours (a version came between them) is removed.

// options are what one run may reach beyond its inputs.
type options struct {
	fetch  bool   // release dates and changelogs from the web
	linker linker // source links; nil leaves changes unlinked
	site   string // the reference's public site, for announcements' links
	// lineage reads a harness's source for where its items came from; nil, or a nil
	// sourcer, keeps the sources its lineage has (lineage.go).
	lineage func(harness string) lineageSourcer
}

// linker says where a harness's public source names what a change is about.
type linker interface {
	link(harness, from, to string, c *Change) *SourceNote
}

// published is what a run starts from.
type published struct {
	snapshots  map[string]map[string]*Snapshot
	diffs      map[string]map[string]*Diff
	versions   map[string]map[string]VersionEntry
	changelogs map[string]map[string]ChangelogNote
	// rescrubbed are the snapshots whose machine paths load named (scrubMachine): published
	// before a rule that names them, their diffs are made again.
	rescrubbed map[buildID]bool
}

// checkBuild says why id's harness or version cannot name a file of the published tree.
func checkBuild(id buildID) error {
	if err := e2e.CheckPathName("harness", id.harness); err != nil {
		return err
	}
	return e2e.CheckPathName(id.harness+" version", id.version)
}

func load(dir string) (published, error) {
	p := published{snapshots: map[string]map[string]*Snapshot{}, diffs: map[string]map[string]*Diff{}, versions: map[string]map[string]VersionEntry{},
		changelogs: map[string]map[string]ChangelogNote{}, rescrubbed: map[buildID]bool{}}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		h := e.Name()
		p.snapshots[h], p.diffs[h], p.versions[h] = map[string]*Snapshot{}, map[string]*Diff{}, map[string]VersionEntry{}
		files, _ := filepath.Glob(filepath.Join(dir, h, "versions", "*.json"))
		for _, f := range files {
			var s Snapshot
			if err := readJSON(f, &s); err != nil {
				return p, err
			}
			// A snapshot's version names the file it is written back to: one that could name a
			// file outside the tree is refused.
			if err := checkBuild(buildID{h, s.Version}); err != nil {
				return p, fmt.Errorf("%s: %w", f, err)
			}
			// A snapshot published while an empty side_requests was left out: its first turn
			// was captured (it has a prompt), so its side requests were read, and there were none.
			if s.Prompt != nil && s.SideRequests == nil {
				s.SideRequests = []any{}
			}
			// What was published before a machine-path rule named it is named now.
			if scrubMachine(&s) {
				p.rescrubbed[buildID{h, s.Version}] = true
			}
			p.snapshots[h][s.Version] = &s
		}
		files, _ = filepath.Glob(filepath.Join(dir, h, "diffs", "*.json"))
		for _, f := range files {
			var d Diff
			if err := readJSON(f, &d); err != nil {
				return p, err
			}
			// What was published of a changelog is bounded as a fetched one is.
			if d.Changelog != nil {
				b := d.Changelog.Bounded()
				d.Changelog = &b
			}
			p.diffs[h][d.From+".."+d.To] = &d
		}
		var cl Changelogs
		if err := readJSON(filepath.Join(dir, h, "changelog.json"), &cl); err == nil {
			p.changelogs[h] = map[string]ChangelogNote{}
			for v, note := range cl.Versions {
				p.changelogs[h][v] = note.Bounded()
			}
		}
		var hi HarnessIndex
		if err := readJSON(filepath.Join(dir, h, "index.json"), &hi); err == nil {
			for _, v := range hi.Versions {
				p.versions[h][v.Version] = v
			}
		}
	}
	return p, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return writeFile(path, buf.Bytes())
}

// writeFile writes data to path, but leaves a file that holds data already as it is: a
// republish rewrites only what changed (refgen -full rewrites every file).
func writeFile(path string, data []byte) error {
	if !fullRender {
		if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, data) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// publish merges the night into dir, and says which versions to announce.
func publish(dir string, n night, opt options) ([]Announcement, error) {
	pub, err := load(dir)
	if err != nil {
		return nil, err
	}
	// The newest version of each harness published before the run: a version newer than it,
	// published by the run, is news.
	before := map[string]string{}
	for h, byVersion := range pub.snapshots {
		before[h] = newestPublished(byVersion)
	}
	// Each harness id and version names a folder or file of the tree: one that would name
	// something outside it (a vendor's feed's ../../x) is refused before anything is written.
	for _, id := range n.builds() {
		if err := checkBuild(id); err != nil {
			return nil, err
		}
	}
	changed := map[buildID]bool{}
	for _, id := range n.builds() {
		if pub.snapshots[id.harness] == nil {
			pub.snapshots[id.harness] = map[string]*Snapshot{}
		}
		old := pub.snapshots[id.harness][id.version]
		// A build's page begins with its capture: terma's checks and the field census of a build
		// no capture took (the night's contracts of a build the frequent check has yet to
		// capture) update a published page, never begin one.
		if old == nil && !n.captures(id) {
			continue
		}
		merged := merge(old, n.tonight(id), n.now)
		if old == nil || !sameSections(old, merged) {
			changed[id] = true
		}
		pub.snapshots[id.harness][id.version] = merged
	}
	// The history holds every build's checks: a build the night did not run keeps its verdict
	// up to date all the same (a check's result corrected, a label renamed).
	for h, byVersion := range pub.snapshots {
		for v, s := range byVersion {
			if changed[buildID{h, v}] {
				continue
			}
			fresh := &Snapshot{Harness: h, Version: v, Binary: s.Binary}
			n.deriveSupport(fresh)
			if fresh.Support != nil && !reflect.DeepEqual(fresh.Support, s.Support) {
				s.Support = fresh.Support
				if s.Capture.Sections == nil {
					s.Capture.Sections = map[string]time.Time{}
				}
				s.Capture.Sections["support"] = n.now
				changed[buildID{h, v}] = true
			}
		}
	}

	var notes changelogs
	var news []Announcement
	// Each harness's files, written once every harness has passed the machine guard.
	var writes []func() error
	var recent []RecentEntry
	index := Index{SchemaVersion: schemaVersion, GeneratedAt: n.now, Harnesses: []HarnessEntry{}}
	for _, h := range sortedHarnesses(pub.snapshots) {
		byVersion := pub.snapshots[h]
		versions := make([]string, 0, len(byVersion))
		for v := range byVersion {
			versions = append(versions, v)
		}
		// Stable, and a total order: 1.2 and 1.2.0 rank alike, and their strings break the tie.
		slices.SortStableFunc(versions, refdiff.VersionCompare)
		meta := metaOf(h)
		var dates map[string]time.Time
		if opt.fetch {
			dates = releaseDates(meta)
		}

		// A build a newer one shows missed something is marked so after its snapshot was
		// read: its diffs either side are made again, not reused from before the marking.
		marks := unobservedMarks(versions, byVersion)
		markUnobserved(versions, byVersion)
		markUnobservedEvents(versions, byVersion)
		markUnobservedFilesystem(versions, byVersion)
		for v, was := range marks {
			if unobservedMark(byVersion[v]) != was {
				changed[buildID{h, v}] = true
			}
		}

		// The vendor's reference tonight goes with tonight's newest build that has its
		// configuration captured; an older build keeps what it got when it was the newest.
		if opt.fetch {
			if d := fetchDocs(h, n.now); d != nil {
				for i := len(versions) - 1; i >= 0; i-- {
					if s := byVersion[versions[i]]; s.Config != nil {
						s.Docs = docsOf(s, d)
						break
					}
				}
			}
		}

		diffs := map[string]*Diff{}
		for i := 1; i < len(versions); i++ {
			from, to := versions[i-1], versions[i]
			key := from + ".." + to
			old := pub.diffs[h][key]
			fresh := !changed[buildID{h, from}] && !changed[buildID{h, to}] && !pub.rescrubbed[buildID{h, from}] && !pub.rescrubbed[buildID{h, to}]
			if old != nil && fresh {
				diffs[key] = old
				continue
			}
			var note *ChangelogNote
			switch {
			case old != nil && old.Changelog != nil:
				note = old.Changelog
			case opt.fetch:
				note = notes.of(h, to)
			}
			d := refdiff.New(byVersion[from], byVersion[to], note, n.now)
			c := sourcelink.Compare(h, from, to)
			if c == "" {
				c = meta.compare(from, to)
			}
			if c != "" {
				d.Source = &DiffSource{Compare: c}
			}
			for j := range d.Changes {
				if old != nil {
					for _, oc := range old.Changes {
						if oc.ID == d.Changes[j].ID && oc.Source != nil {
							d.Changes[j].Source = oc.Source
						}
					}
				}
				if d.Changes[j].Source == nil && opt.linker != nil {
					d.Changes[j].Source = opt.linker.link(h, from, to, &d.Changes[j])
				}
			}
			diffs[key] = d
		}

		entries := make([]VersionEntry, 0, len(versions))
		for i, v := range versions {
			s := byVersion[v]
			e := VersionEntry{Version: v, CapturedAt: s.CapturedAt, Platforms: platformsOf(s), Support: supportOf(s),
				Categories: map[string]int{}, Impact: map[string]int{"breaking": 0, "degrading": 0, "notable": 0, "informational": 0}, Highlights: []string{},
				Partial: s.Capture.Partial}
			var bin binaryView
			view(s.Binary, &bin)
			if bin.Signing != nil {
				e.Signing = bin.Signing.Status.ptr()
			}
			if t, ok := dates[v]; ok {
				e.ReleasedAt = &t
			} else if prev, ok := pub.versions[h][v]; ok {
				e.ReleasedAt = prev.ReleasedAt
			}
			if i > 0 {
				prev := versions[i-1]
				e.Previous = &prev
				d := diffs[prev+".."+v]
				e.Changes, e.Categories, e.Impact, e.Undocumented = d.Summary.Changes, d.Summary.Categories, d.Summary.Impact, d.Summary.Undocumented
				e.Highlights = refdiff.Highlights(d, 3)
			}
			entries = append(entries, e)
		}
		// Newest first, as a person reads a changelog.
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}

		entry := HarnessEntry{ID: h, Name: meta.name, Vendor: meta.vendor, Homepage: meta.homepage, Source: meta.source(),
			Changelog: meta.changelog, Latest: versions[len(versions)-1], Support: entries[0].Support, VersionsTracked: len(versions)}
		entry.FirstTracked = firstTracked(entries)
		if len(versions) > 1 {
			from, to := versions[len(versions)-2], versions[len(versions)-1]
			d := diffs[from+".."+to]
			entry.LastChange = &LastChange{From: from, To: to, At: byVersion[to].CapturedAt, ReleasedAt: entries[0].ReleasedAt,
				Changes: d.Summary.Changes, Categories: d.Summary.Categories}
		}
		index.Harnesses = append(index.Harnesses, entry)

		cl := Changelogs{SchemaVersion: schemaVersion, Harness: h, Versions: map[string]ChangelogNote{}}
		for v, note := range pub.changelogs[h] {
			cl.Versions[v] = note
		}
		for _, d := range diffs {
			if d.Changelog != nil {
				cl.Versions[d.To] = *d.Changelog
			}
		}
		// Nothing that names the machine is written: the guard reads every observed value of the
		// harness's snapshots and diffs before any file of the run is.
		if err := machineGuard(h, byVersion, diffs); err != nil {
			return nil, err
		}
		var src lineageSourcer
		if opt.lineage != nil {
			src = opt.lineage(h)
		}
		writes = append(writes, func() error {
			if err := writeHarness(filepath.Join(dir, h), entry, entries, byVersion, diffs, cl); err != nil {
				return err
			}
			return writeLineage(filepath.Join(dir, h), h, versions, byVersion, src)
		})
		news = append(news, announce(opt.site, entry, entries, diffs, byVersion, before[h])...)
		for _, e := range entries {
			if e.Previous != nil {
				recent = append(recent, RecentEntry{Harness: h, Name: meta.name, VersionEntry: e})
			}
		}
	}
	for _, write := range writes {
		if err := write(); err != nil {
			return nil, err
		}
	}
	if err := writeJSON(filepath.Join(dir, "recent.json"), recentOf(recent, n.now)); err != nil {
		return nil, err
	}
	return news, writeJSON(filepath.Join(dir, "index.json"), index)
}

// recentLimit bounds the feed.
const recentLimit = 200

// recentOf is the feed: the newest versions across every harness, newest release first.
func recentOf(entries []RecentEntry, now time.Time) Recent {
	// A version whose release date is known goes by it; one whose is not (an app no registry
	// dates) after them, by when it was captured: a backfill captures old builds tonight.
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch {
		case a.ReleasedAt != nil && b.ReleasedAt != nil:
			return a.ReleasedAt.After(*b.ReleasedAt)
		case a.ReleasedAt != nil || b.ReleasedAt != nil:
			return a.ReleasedAt != nil
		}
		return a.CapturedAt.After(b.CapturedAt)
	})
	if len(entries) > recentLimit {
		entries = entries[:recentLimit]
	}
	if entries == nil {
		entries = []RecentEntry{}
	}
	return Recent{SchemaVersion: schemaVersion, GeneratedAt: now, Versions: entries}
}

// writeHarness writes one harness's tree, and removes what no longer belongs in it.
func writeHarness(dir string, entry HarnessEntry, entries []VersionEntry, snaps map[string]*Snapshot, diffs map[string]*Diff, cl Changelogs) error {
	for v, s := range snaps {
		if err := writeJSON(filepath.Join(dir, "versions", v+".json"), s); err != nil {
			return err
		}
		if md := promptMarkdown(entry.Name, s); md != "" {
			if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
				return err
			}
			if err := writeFile(filepath.Join(dir, "prompts", v+".md"), []byte(md)); err != nil {
				return err
			}
		}
	}
	existing, _ := filepath.Glob(filepath.Join(dir, "diffs", "*.json"))
	for _, f := range existing {
		if _, ok := diffs[strings.TrimSuffix(filepath.Base(f), ".json")]; !ok {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	for key, d := range diffs {
		if err := writeJSON(filepath.Join(dir, "diffs", key+".json"), d); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(dir, "changelog.json"), cl); err != nil {
		return err
	}
	if err := writeTimeline(dir, timelineOf(entry.ID, entries, diffs), timelineSteps); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "index.json"), HarnessIndex{SchemaVersion: schemaVersion, Harness: entry, Versions: entries})
}

// timelineOf is every step of a harness's versions, newest first: each version's changes from
// the one before it, without their values, so a range of versions reads as what each step did.
func timelineOf(harness string, entries []VersionEntry, diffs map[string]*Diff) Timeline {
	t := Timeline{SchemaVersion: schemaVersion, Harness: harness, Steps: []TimelineStep{}}
	for _, e := range entries {
		if e.Previous == nil {
			continue
		}
		d := diffs[*e.Previous+".."+e.Version]
		step := TimelineStep{Version: e.Version, Previous: *e.Previous, Changes: make([]TimelineChange, 0, len(d.Changes))}
		for _, c := range d.Changes {
			step.Changes = append(step.Changes, TimelineChange{ID: c.ID, Category: c.Category, Op: c.Op, Title: c.Title, Impact: c.Impact, Documented: c.Documented})
		}
		t.Steps = append(t.Steps, step)
	}
	return t
}

// timelineSteps is how many steps timeline.json holds, and each archive at most: the newest
// steps, which a harness's page reads, in one bounded file, and every older one in a file of its
// own (refgen -timeline-steps).
var timelineSteps = 25

// writeTimeline writes t's newest steps (at most per) to timeline.json, and the older ones to
// timeline-1.json, timeline-2.json and on, per steps each counted from the oldest, so a full
// archive's steps never move; an archive no longer needed is removed. A harness of per steps or
// fewer has every step in timeline.json, and no archives.
func writeTimeline(dir string, t Timeline, per int) error {
	if per < 1 {
		per = 1
	}
	head, older := t, []TimelineStep(nil)
	if len(t.Steps) > per {
		head.Steps, older = t.Steps[:per], t.Steps[per:]
	}
	// Oldest first, in archives of per steps from the oldest.
	slices.Reverse(older)
	n := 0
	for len(older) > 0 {
		n++
		chunk := older[:min(per, len(older))]
		older = older[len(chunk):]
		steps := slices.Clone(chunk)
		slices.Reverse(steps) // newest first, as every timeline lists them
		name := fmt.Sprintf("timeline-%d.json", n)
		head.Archives = append(head.Archives, TimelineArchive{File: name, First: chunk[0].Version, Last: chunk[len(chunk)-1].Version, Steps: len(chunk)})
		if err := writeJSON(filepath.Join(dir, name), Timeline{SchemaVersion: t.SchemaVersion, Harness: t.Harness, Steps: steps}); err != nil {
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

// promptMarkdown is a build's prompts as one text a person reads, "" where none was captured.
func promptMarkdown(name string, s *Snapshot) string {
	if s.Prompt == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s %s: system prompt\n\nCaptured %s by Terma's Harness Reference. Paths, dates and ids are placeholders.\n",
		name, s.Version, s.CapturedAt.Format("2006-01-02"))
	block := func(heading string, list []promptBlock) {
		for i, b := range list {
			title := heading
			if len(list) > 1 {
				title = fmt.Sprintf("%s %d", heading, i+1)
			}
			fmt.Fprintf(&sb, "\n## %s\n\n%s\n", title, b.Text)
		}
	}
	var p promptView
	view(s.Prompt, &p)
	block("System", p.System)
	block("Injected context", p.Context)
	if p.Interactive != nil {
		block("Interactive session: system", p.Interactive.System)
		block("Interactive session: injected context", p.Interactive.Context)
	}
	return sb.String()
}

func sameSections(a, b *Snapshot) bool {
	at, bt := a.Tree(), b.Tree()
	for _, sec := range sections {
		if !reflect.DeepEqual(at[sec], bt[sec]) {
			return false
		}
	}
	return true
}

// The few fields of a snapshot's generic sections publishing reads, typed: each a view of the
// section's JSON (view), its other keys left out.
type (
	// binaryView is the binary section's signature verdict.
	binaryView struct {
		Signing *struct {
			Status jsonString `json:"status"`
		} `json:"signing"`
	}
	// promptView is the prompt section's texts: the system prompt's blocks, the injected
	// context's, and an interactive session's.
	promptView struct {
		System      []promptBlock `json:"system"`
		Context     []promptBlock `json:"context"`
		Interactive *struct {
			System  []promptBlock `json:"system"`
			Context []promptBlock `json:"context"`
		} `json:"interactive"`
	}
	promptBlock struct {
		Text string `json:"text"`
	}
	// configView is where the config section's options were read from, and their keys.
	configView struct {
		Source  string                     `json:"source"`
		Options map[string]json.RawMessage `json:"options"`
	}
	// telemetryView is the network section's vendor telemetry: each endpoint's events.
	telemetryView map[string]struct {
		Events map[string]json.RawMessage `json:"events"`
	}
)

// jsonString is a JSON string where the value is one: null or any other kind reads as none.
type jsonString struct {
	s  string
	ok bool
}

func (j *jsonString) UnmarshalJSON(data []byte) error {
	j.ok = json.Unmarshal(data, &j.s) == nil && string(data) != "null"
	return nil
}

// ptr is the string, nil where there was none.
func (j jsonString) ptr() *string {
	if !j.ok {
		return nil
	}
	s := j.s
	return &s
}

// view reads v, generic JSON or any value, into out, a typed view of the fields a reader
// needs: a field whose JSON is of another kind than out's is left zero, the rest still read.
func view(v, out any) {
	if data, err := json.Marshal(v); err == nil {
		_ = json.Unmarshal(data, out)
	}
}

func jsonOf(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

func sortedHarnesses(m map[string]map[string]*Snapshot) []string {
	out := make([]string, 0, len(m))
	for h, vs := range m {
		if len(vs) > 0 {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

func platformsOf(s *Snapshot) []string {
	seen := map[string]bool{}
	if s.Platform != "" {
		seen[s.Platform] = true
	}
	if s.Support != nil {
		for _, c := range s.Support.Capabilities {
			if c.Platform != "" {
				seen[c.Platform] = true
			}
		}
	}
	// A platform the build's files and processes were traced on is one it was captured on.
	for p := range s.Filesystem {
		seen[p] = true
	}
	return sortedKeys(seen)
}

func supportOf(s *Snapshot) string {
	if s.Support != nil {
		return s.Support.Status
	}
	return "untested"
}

// firstTracked is the date of the oldest version tracked: its release, or else when it was
// first captured.
func firstTracked(entries []VersionEntry) string {
	var first time.Time
	for _, e := range entries {
		t := e.CapturedAt
		if e.ReleasedAt != nil {
			t = *e.ReleasedAt
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	if first.IsZero() {
		return ""
	}
	return first.Format("2006-01-02")
}

// markUnobserved says, of each build whose settings were read from minified code, the
// top-level settings its reading did not find though builds before and after it have them:
// the reader missed them there (a block of the schema it did not follow), not the build. A
// diff takes them for not observed, rather than removed and added again.
func markUnobserved(versions []string, byVersion map[string]*Snapshot) {
	tops := make([]map[string]bool, len(versions))
	approx := make([]bool, len(versions))
	for i, v := range versions {
		cfg := byVersion[v].Config
		var c configView
		view(cfg, &c)
		approx[i] = strings.Contains(c.Source, "minified")
		tops[i] = map[string]bool{}
		for k := range c.Options {
			top, _, _ := strings.Cut(k, ".")
			top, _, _ = strings.Cut(top, "[")
			tops[i][top] = true
		}
		delete(cfg, "unobserved")
	}
	sets := make([]map[string]bool, len(versions))
	for i := range versions {
		if approx[i] {
			sets[i] = tops[i]
		}
	}
	for i, gaps := range missedBetween(sets) {
		if len(tops[i]) > 0 && len(gaps) > 0 {
			byVersion[versions[i]].Config["unobserved"] = gaps
		}
	}
}

// unobservedMarks is each version's unobservedMark.
func unobservedMarks(versions []string, byVersion map[string]*Snapshot) map[string]string {
	out := make(map[string]string, len(versions))
	for _, v := range versions {
		out[v] = unobservedMark(byVersion[v])
	}
	return out
}

// unobservedMark is what a snapshot says it did not observe, its settings', telemetry
// endpoints' and filesystem platforms' "unobserved", as one comparable string.
func unobservedMark(s *Snapshot) string {
	marks := map[string]any{}
	if u, ok := s.Config["unobserved"]; ok {
		marks["config"] = u
	}
	tel, _ := s.Network["telemetry"].(map[string]any)
	for ep, raw := range tel {
		if m, ok := raw.(map[string]any); ok && m["unobserved"] != nil {
			marks["telemetry "+ep] = m["unobserved"]
		}
	}
	for plat, raw := range s.Filesystem {
		if m, ok := raw.(map[string]any); ok && m["unobserved"] != nil {
			marks["filesystem "+plat] = m["unobserved"]
		}
	}
	data, _ := json.Marshal(marks) // map keys sorted
	return string(data)
}

// missedBetween is, for each of sets (one a version, in order; nil one that takes no part), the
// keys some set before it and some set after it have and it has not, sorted: nil for a nil set.
// Each key's first and last set say it, so it takes one pass over the sets and one over the
// keys for each, not a pass over every other set for each.
func missedBetween(sets []map[string]bool) [][]string {
	first, last := map[string]int{}, map[string]int{}
	for i, s := range sets {
		for k := range s {
			if _, seen := first[k]; !seen {
				first[k] = i
			}
			last[k] = i
		}
	}
	keys := make([]string, 0, len(first))
	for k := range first {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][]string, len(sets))
	for i, s := range sets {
		if s == nil {
			continue
		}
		for _, k := range keys {
			if first[k] < i && last[k] > i && !s[k] {
				out[i] = append(out[i], k)
			}
		}
	}
	return out
}

// markUnobservedEvents says, of each build's vendor telemetry endpoint, the events builds
// before and after it reported there and its run did not: an event a run reports only
// sometimes (a sampled sweep, a feature check), not one the build stopped sending.
func markUnobservedEvents(versions []string, byVersion map[string]*Snapshot) {
	type seen = map[string]map[string]bool // endpoint → event names
	per := make([]seen, len(versions))
	for i, v := range versions {
		per[i] = seen{}
		tel, _ := byVersion[v].Network["telemetry"].(map[string]any)
		for _, raw := range tel {
			if m, ok := raw.(map[string]any); ok {
				delete(m, "unobserved")
			}
		}
		var endpoints telemetryView
		view(tel, &endpoints)
		for ep, e := range endpoints {
			per[i][ep] = map[string]bool{}
			for name := range e.Events {
				per[i][ep][name] = true
			}
		}
	}
	missed := map[string][][]string{} // endpoint → each version's gaps there
	for i, v := range versions {
		tel, _ := byVersion[v].Network["telemetry"].(map[string]any)
		for ep, raw := range tel {
			if _, done := missed[ep]; !done {
				sets := make([]map[string]bool, len(versions))
				for j := range versions {
					sets[j] = per[j][ep]
				}
				missed[ep] = missedBetween(sets)
			}
			if gaps := missed[ep][i]; len(gaps) > 0 {
				raw.(map[string]any)["unobserved"] = gaps
			}
		}
	}
}

// captures reports a build the night captured, or set out to: a profile of it, or an attempt.
func (n night) captures(id buildID) bool {
	if slices.Contains(n.attempted, id) {
		return true
	}
	for _, p := range n.profiles {
		if p.Harness == id.harness && p.Version == id.version {
			return true
		}
	}
	return false
}
