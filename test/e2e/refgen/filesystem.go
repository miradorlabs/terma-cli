package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The filesystem section: each platform's files profile of a build (the probe "filesystem",
// test/e2e/filetrace.go), from the profile rows of its Test…ProfileFiles scenarios, by
// platform. A files profile is a scenario of its own, traced on its own machine (Linux under
// strace, macOS under eslogger), so its row is never merged into the build's first turn; each
// platform's is kept whichever platform the rest of the snapshot was read on. Its paths are
// normalized when the scenario records them; here only the run's literals the profile names
// are replaced again, not the volatile patterns, which would take a listener's fixed port for
// the run's.

// isFilesScenario reports a files profile's row.
func isFilesScenario(p e2e.Profile) bool { return strings.HasSuffix(p.Scenario, "ProfileFiles") }

// deriveFilesystem fills s's filesystem section from the night's files profiles of the build:
// each platform's whole one over a failed one.
func (n night) deriveFilesystem(s *Snapshot, id buildID) {
	failed := map[string]bool{}
	for _, p := range n.profiles {
		if p.Harness != id.harness || p.Version != id.version {
			continue
		}
		raw, ok := p.Probes["filesystem"]
		if !ok {
			continue
		}
		var probe map[string]any
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		platform, _ := probe["platform"].(string)
		if platform == "" {
			platform = p.Platform
		}
		if s.Filesystem == nil {
			s.Filesystem = map[string]any{}
		}
		if _, have := s.Filesystem[platform]; have && (p.Failed || !failed[platform]) {
			continue
		}
		s.Filesystem[platform] = literalsReplaced(probe, p)
		failed[platform] = p.Failed
	}
}

// literalsReplaced is v with the run's literals that p names in their placeholders.
func literalsReplaced(v any, p e2e.Profile) any {
	lits := make([]string, 0, len(p.Placeholders))
	for lit := range p.Placeholders {
		if len(lit) >= 4 {
			lits = append(lits, lit)
		}
	}
	sort.Slice(lits, func(i, j int) bool { return len(lits[i]) > len(lits[j]) })
	var version *regexp.Regexp
	if len(p.Version) >= 5 {
		version = regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(p.Version) + `($|[^\w])`)
	}
	return walkStrings(v, func(s string) string {
		for _, lit := range lits {
			s = strings.ReplaceAll(s, lit, p.Placeholders[lit])
		}
		if version != nil {
			s = version.ReplaceAllString(s, "${1}<version>${2}")
		}
		return s
	})
}

// mergeFilesystem is the filesystem section with tonight's platforms over old's: a night that
// traced on one platform keeps another's from before.
func mergeFilesystem(old, tonight any) any {
	o, _ := old.(map[string]any)
	t, _ := tonight.(map[string]any)
	if o == nil || t == nil {
		return tonight
	}
	out := map[string]any{}
	for k, v := range o {
		out[k] = v
	}
	for k, v := range t {
		out[k] = v
	}
	return out
}

// fsParts are the parts of a files profile whose keys a run can miss.
var fsParts = []struct{ name, under string }{
	{"paths", ""}, {"exec", ""}, {"listening", "sockets"}, {"connections", "sockets"}, {"unix", "sockets"},
}

// markUnobservedFilesystem says, of each build's files profile on each platform, the paths,
// commands and sockets builds before and after it have there and its run did not: what a run
// does only sometimes (a cache it found fresh, a cleanup it skipped), not what the build stopped
// doing. A diff takes them for not observed.
func markUnobservedFilesystem(versions []string, byVersion map[string]*Snapshot) {
	type keys = map[string]map[string]bool // part → keys
	partOf := func(probe map[string]any, name, under string) map[string]any {
		m := probe
		if under != "" {
			m, _ = probe[under].(map[string]any)
		}
		v, _ := m[name].(map[string]any)
		return v
	}
	platforms := map[string]bool{}
	for _, v := range versions {
		for plat, raw := range byVersion[v].Filesystem {
			platforms[plat] = true
			if m, ok := raw.(map[string]any); ok {
				delete(m, "unobserved")
			}
		}
	}
	for plat := range platforms {
		per := make([]keys, len(versions))
		for i, v := range versions {
			probe, _ := byVersion[v].Filesystem[plat].(map[string]any)
			if probe == nil {
				continue
			}
			per[i] = keys{}
			for _, part := range fsParts {
				per[i][part.name] = map[string]bool{}
				for k := range partOf(probe, part.name, part.under) {
					per[i][part.name][k] = true
				}
			}
		}
		for i, v := range versions {
			if per[i] == nil {
				continue
			}
			gaps := map[string]any{}
			for _, part := range fsParts {
				before, after := map[string]bool{}, map[string]bool{}
				for j := range i {
					for k := range per[j][part.name] {
						before[k] = true
					}
				}
				for j := i + 1; j < len(versions); j++ {
					for k := range per[j][part.name] {
						after[k] = true
					}
				}
				var missed []string
				for k := range before {
					if after[k] && !per[i][part.name][k] {
						missed = append(missed, k)
					}
				}
				if len(missed) > 0 {
					sort.Strings(missed)
					gaps[part.name] = missed
				}
			}
			if len(gaps) > 0 {
				byVersion[v].Filesystem[plat].(map[string]any)["unobserved"] = gaps
			}
		}
	}
}
