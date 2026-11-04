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
	"sort"
	"strings"
	"time"
)

// Publishing: the night's snapshots merged into the published ones, each harness's versions
// compared in order, and the whole written back. A diff whose two snapshots the night did not
// change is kept as it was (with what its changelog and source said then); one the night did
// is made again. A diff no longer between neighbours (a version came between them) is removed.

// options are what one run may reach beyond its inputs.
type options struct {
	fetch  bool   // release dates and changelogs from the web
	linker linker // source links; nil leaves changes unlinked
}

// linker says where a harness's public source names what a change is about.
type linker interface {
	link(harness, from, to string, c *Change) *SourceNote
}

// published is what a run starts from.
type published struct {
	snapshots map[string]map[string]*Snapshot
	diffs     map[string]map[string]*Diff
	versions  map[string]map[string]VersionEntry
}

func load(dir string) (published, error) {
	p := published{snapshots: map[string]map[string]*Snapshot{}, diffs: map[string]map[string]*Diff{}, versions: map[string]map[string]VersionEntry{}}
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
			p.snapshots[h][s.Version] = &s
		}
		files, _ = filepath.Glob(filepath.Join(dir, h, "diffs", "*.json"))
		for _, f := range files {
			var d Diff
			if err := readJSON(f, &d); err != nil {
				return p, err
			}
			p.diffs[h][d.From+".."+d.To] = &d
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// publish merges the night into dir.
func publish(dir string, n night, opt options) error {
	pub, err := load(dir)
	if err != nil {
		return err
	}
	changed := map[buildID]bool{}
	for _, id := range n.builds() {
		if pub.snapshots[id.harness] == nil {
			pub.snapshots[id.harness] = map[string]*Snapshot{}
		}
		old := pub.snapshots[id.harness][id.version]
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
			fresh := &Snapshot{Harness: h, Version: v}
			n.deriveSupport(fresh)
			if fresh.Support != nil && !reflect.DeepEqual(jsonOf(fresh.Support), jsonOf(s.Support)) {
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
	index := Index{SchemaVersion: schemaVersion, GeneratedAt: n.now, Harnesses: []HarnessEntry{}}
	for _, h := range sortedHarnesses(pub.snapshots) {
		byVersion := pub.snapshots[h]
		versions := make([]string, 0, len(byVersion))
		for v := range byVersion {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool { return versionLess(versions[i], versions[j]) })
		meta := metaOf(h)
		var dates map[string]time.Time
		if opt.fetch {
			dates = releaseDates(meta)
		}

		diffs := map[string]*Diff{}
		for i := 1; i < len(versions); i++ {
			from, to := versions[i-1], versions[i]
			key := from + ".." + to
			old := pub.diffs[h][key]
			if old != nil && !changed[buildID{h, from}] && !changed[buildID{h, to}] {
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
			d := newDiff(byVersion[from], byVersion[to], note, n.now)
			if c := meta.compare(from, to); c != "" {
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
				Categories: map[string]int{}, Impact: map[string]int{"breaking": 0, "degrading": 0, "notable": 0, "informational": 0}, Highlights: []string{}}
			if sig, ok := s.Binary["signing"].(map[string]any); ok {
				if st, ok := sig["status"].(string); ok {
					e.Signing = &st
				}
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
				e.Highlights = highlights(d, 3)
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
			entry.LastChange = &LastChange{From: from, To: to, At: byVersion[to].CapturedAt, Changes: d.Summary.Changes, Categories: d.Summary.Categories}
		}
		index.Harnesses = append(index.Harnesses, entry)

		if err := writeHarness(filepath.Join(dir, h), entry, entries, byVersion, diffs); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(dir, "index.json"), index)
}

// writeHarness writes one harness's tree, and removes what no longer belongs in it.
func writeHarness(dir string, entry HarnessEntry, entries []VersionEntry, snaps map[string]*Snapshot, diffs map[string]*Diff) error {
	for v, s := range snaps {
		if err := writeJSON(filepath.Join(dir, "versions", v+".json"), s); err != nil {
			return err
		}
		if md := promptMarkdown(entry.Name, s); md != "" {
			if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "prompts", v+".md"), []byte(md), 0o644); err != nil {
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
	return writeJSON(filepath.Join(dir, "index.json"), HarnessIndex{SchemaVersion: schemaVersion, Harness: entry, Versions: entries})
}

// promptMarkdown is a build's prompts as one text a person reads, "" where none was captured.
func promptMarkdown(name string, s *Snapshot) string {
	if s.Prompt == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s %s: system prompt\n\nCaptured %s by Terma's Harness Reference. Paths, dates and ids are placeholders.\n",
		name, s.Version, s.CapturedAt.Format("2006-01-02"))
	block := func(heading string, v any) {
		list, _ := v.([]any)
		for i, b := range list {
			m, _ := b.(map[string]any)
			text, _ := m["text"].(string)
			title := heading
			if len(list) > 1 {
				title = fmt.Sprintf("%s %d", heading, i+1)
			}
			fmt.Fprintf(&sb, "\n## %s\n\n%s\n", title, text)
		}
	}
	block("System", s.Prompt["system"])
	block("Injected context", s.Prompt["context"])
	if in, ok := s.Prompt["interactive"].(map[string]any); ok {
		block("Interactive session: system", in["system"])
		block("Interactive session: injected context", in["context"])
	}
	return sb.String()
}

func sameSections(a, b *Snapshot) bool {
	at, bt := a.tree(), b.tree()
	for _, sec := range sections {
		if !reflect.DeepEqual(at[sec], bt[sec]) {
			return false
		}
	}
	return true
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
	if caps, ok := s.Support["capabilities"].([]any); ok {
		for _, c := range caps {
			if p, _ := c.(map[string]any)["platform"].(string); p != "" {
				seen[p] = true
			}
		}
	}
	return sortedKeys(seen)
}

func supportOf(s *Snapshot) string {
	if st, ok := s.Support["status"].(string); ok {
		return st
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
