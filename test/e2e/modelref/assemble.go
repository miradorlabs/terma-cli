package main

import (
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
)

// Assembling a model from what each source says of it. Which models the reference follows is
// the vendors' own lists: every model a vendor source that was read lists, and every model
// published before whose vendor sources were not read this time (an outage is no news). A fact
// takes the most trusted source's value (sourceRank); a source not read this time keeps saying
// what it said before, from the snapshot's provenance, so a catalog's outage never reads as a
// fact going away.

// run is one run's reading: every observation, and which sources were read.
type run struct {
	now  time.Time
	obs  []observation
	read map[string]bool // source → read whole this run
	// wanted are the vendors read this run; none named is every one.
	wanted map[string]bool
	// failed are the sources tried and not read.
	failed map[string]string
}

// followed are the ids of the models the reference follows after this run, by vendor, and
// whether each is listed by its vendor now.
// covers says whether the run read what sources say of vendor's models.
func (r *run) covers(vendor string) bool { return len(r.wanted) == 0 || r.wanted[vendor] }

func followed(r *run, prev map[string]*Snapshot) map[string]bool {
	listed := map[string]bool{}
	for _, o := range r.obs {
		if o.listed {
			listed[o.id] = true
		}
	}
	out := map[string]bool{}
	for id := range listed {
		out[id] = true
	}
	for id, s := range prev {
		if out[id] {
			continue
		}
		// Still listed unless a vendor source that lists models was read this time and did not
		// list it.
		readSome := false
		for src := range s.Sources {
			if strings.HasPrefix(src, s.Vendor+"-") && r.read[src] && r.covers(s.Vendor) {
				readSome = true
			}
		}
		out[id] = s.Listed && !readSome
	}
	return out
}

// assemble builds a model's snapshot from this run's observations and its previous snapshot.
func assemble(id, vendorID string, r *run, prev *Snapshot, listed bool) *Snapshot {
	s := &Snapshot{SchemaVersion: schemaVersion, ID: id, Vendor: vendorID, FirstSeen: r.now, Listed: listed,
		Facts: map[string]any{}, Provenance: map[string]map[string]any{}, Sources: map[string]string{},
		Docs: map[string]Page{}, Harnesses: []HarnessUse{}, Disagreements: []string{}}
	if prev != nil {
		s.FirstSeen = prev.FirstSeen
	}
	// What each source says of the model: this run's reading, or, for a source not read this
	// time, what it said before.
	bySource := map[string]map[string]any{}
	key, undated := norm(id), norm(reDateSuffix.ReplaceAllString(id, ""))
	exact := map[string]bool{}
	for _, o := range r.obs {
		if o.vendor == vendorID && norm(o.id) == key {
			exact[o.source] = true
		}
	}
	for _, o := range r.obs {
		// A catalog that names a dated model by its undated name (OpenRouter's claude-opus-4.5
		// for claude-opus-4-5-20251101) says it of the dated one, where it names no other.
		match := norm(o.id) == key || !o.listed && !exact[o.source] && undated != key && norm(o.id) == undated
		if o.vendor != vendorID || !match {
			continue
		}
		f := bySource[o.source]
		if f == nil {
			f = map[string]any{}
			bySource[o.source] = f
		}
		for k, v := range o.facts {
			if _, ok := f[k]; !ok {
				f[k] = v
			}
		}
		if _, ok := s.Sources[o.source]; !ok {
			// A source's first page of the model (its own page, before a list that names it).
			s.Sources[o.source] = o.url
		}
		for name, p := range o.docs {
			s.Docs[name] = p
		}
		if o.listing != nil {
			if s.API == nil {
				s.API = &API{}
			}
			s.API.Listing = o.listing
		}
	}
	if prev != nil {
		for src, url := range prev.Sources {
			if r.read[src] && r.covers(vendorID) {
				continue
			}
			s.Sources[src] = url
			for fact, vals := range prev.Provenance {
				if v, ok := vals[src]; ok {
					if bySource[src] == nil {
						bySource[src] = map[string]any{}
					}
					bySource[src][fact] = v
				}
			}
			if strings.HasSuffix(src, "-docs") {
				for name, p := range prev.Docs {
					if _, ok := s.Docs[name]; !ok {
						s.Docs[name] = p
					}
				}
			}
			if strings.HasSuffix(src, "-api") && prev.API != nil && prev.API.Listing != nil {
				if s.API == nil {
					s.API = &API{}
				}
				s.API.Listing = prev.API.Listing
			}
		}
		if prev.API != nil && prev.API.Probes != nil {
			if s.API == nil {
				s.API = &API{}
			}
			// A copy: the run derives what it shares with other models (TokenizerLike) on it, and
			// the snapshot before must stay as it was, to compare with.
			kept := *prev.API.Probes
			kept.TokenizerLike = slices.Clone(kept.TokenizerLike)
			s.API.Probes = &kept
		}
	}
	sources := make([]string, 0, len(bySource))
	for src := range bySource {
		sources = append(sources, src)
	}
	sort.Slice(sources, func(i, j int) bool {
		if a, b := sourceRank(sources[i]), sourceRank(sources[j]); a != b {
			return a < b
		}
		return sources[i] < sources[j]
	})
	for _, src := range sources {
		for fact, v := range bySource[src] {
			if s.Provenance[fact] == nil {
				s.Provenance[fact] = map[string]any{}
			}
			s.Provenance[fact][src] = v
			if _, ok := s.Facts[fact]; !ok {
				s.Facts[fact] = v
			}
		}
	}
	for _, fact := range factOrder {
		if disagree(s.Provenance[fact]) {
			s.Disagreements = append(s.Disagreements, fact)
		}
	}
	s.Name = id
	if n, ok := s.Facts[factName].(string); ok {
		s.Name = n
	}
	s.Family = family(id)
	return s
}

// disagree says whether sources state different values of a fact: numbers that differ by more
// than a vendor's rounding (1M against 1,048,576), dates where neither is the other to a coarser
// precision (2026-06 agrees with 2026-06-30), anything else that differs. Lists are not
// compared: catalogs name modalities and features each their own way.
func disagree(vals map[string]any) bool {
	var seen []any
	for _, v := range vals {
		switch v.(type) {
		case []string, map[string]string, []any, map[string]any:
			return false
		}
		for _, w := range seen {
			if !agree(v, w) {
				return true
			}
		}
		seen = append(seen, v)
	}
	return false
}

func agree(a, b any) bool {
	if x, ok := num(a); ok {
		if y, ok := num(b); ok {
			if x == y {
				return true
			}
			round := math.Mod(x, 1000) == 0 || math.Mod(y, 1000) == 0
			return round && math.Abs(x-y) <= 0.05*math.Max(x, y)
		}
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			if isoDate(x) != "" && isoDate(y) != "" {
				return strings.HasPrefix(x, y) || strings.HasPrefix(y, x)
			}
			return strings.EqualFold(x, y)
		}
	}
	return reflect.DeepEqual(a, b)
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// family is a model's line: its id without version numbers, dates and release qualifiers.
// claude-fable-5-2 is claude-fable, gpt-6.1-sol gpt-sol, gemini-3.5-flash-lite gemini-flash-lite,
// claude-3-7-sonnet-20250219 claude-sonnet.
func family(id string) string {
	var keep []string
	for _, t := range strings.Split(strings.ToLower(id), "-") {
		if versionToken(t) || qualifier[t] {
			continue
		}
		keep = append(keep, t)
	}
	if len(keep) == 0 {
		return id
	}
	return strings.Join(keep, "-")
}

var qualifier = map[string]bool{"preview": true, "latest": true, "exp": true, "experimental": true}

// versionToken says whether a piece of an id is a version (5, 6.1, 4.20) or a date (0309,
// 20251101).
func versionToken(t string) bool {
	if t == "" {
		return false
	}
	for _, c := range t {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// version is a model's version: the numbers in its id, a date's left out.
func version(id string) []int {
	var out []int
	for _, t := range strings.Split(id, "-") {
		if !versionToken(t) || len(t) >= 4 && !strings.Contains(t, ".") {
			continue
		}
		for p := range strings.SplitSeq(t, ".") {
			n := 0
			for _, c := range p {
				n = n*10 + int(c-'0')
			}
			out = append(out, n)
		}
	}
	return out
}

// lineage sets, on each model, the model before it in its family (a lower version of the same
// vendor's same family, the newest such; at an equal version the one released earlier) and the
// one after it.
func lineage(snaps map[string]*Snapshot) (pred, succ map[string]string) {
	pred, succ = map[string]string{}, map[string]string{}
	byFamily := map[string][]*Snapshot{}
	for _, s := range snaps {
		if len(version(s.ID)) > 0 {
			byFamily[s.Vendor+"/"+s.Family] = append(byFamily[s.Vendor+"/"+s.Family], s)
		}
	}
	for _, list := range byFamily {
		sort.Slice(list, func(i, j int) bool { return older(list[i], list[j]) })
		for i := 1; i < len(list); i++ {
			pred[list[i].ID] = list[i-1].ID
			succ[list[i-1].ID] = list[i].ID
		}
	}
	return pred, succ
}

func older(a, b *Snapshot) bool {
	if c := slices.Compare(version(a.ID), version(b.ID)); c != 0 {
		return c < 0
	}
	ra, _ := a.Facts[factReleasedAt].(string)
	rb, _ := b.Facts[factReleasedAt].(string)
	if ra != rb {
		return ra < rb
	}
	return a.ID < b.ID
}
