package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// The join with the Harness Reference (docs/harnesses): which coding-agent harness builds use a
// model, and how. Each build's snapshot says the model a session starts on, the aliases it
// offers, and the models its compaction, subagents, fallback and side requests ask for; the
// reference reads every tracked build, oldest first, and keeps for each model and each way a
// harness uses it the builds that do.

// harnessSnapshot is what the join reads of a build's snapshot.
type harnessSnapshot struct {
	Model *struct {
		Default *string           `json:"default"`
		Aliases map[string]string `json:"aliases"`
		Params  map[string]any    `json:"params"`
	} `json:"model"`
	Behavior *struct {
		Compaction *struct {
			Model *string `json:"model"`
		} `json:"compaction"`
		Subagents *struct {
			Model *string `json:"model"`
		} `json:"subagents"`
		Retries *struct {
			FallbackModel *string `json:"fallback_model"`
		} `json:"retries"`
	} `json:"behavior"`
	SideRequests []struct {
		Model *string `json:"model"`
	} `json:"side_requests"`
}

// harnessUses reads the Harness Reference at dir and says, by model id as each harness names
// it (normalized, norm), every way a harness's builds use it.
func harnessUses(dir string) (map[string][]HarnessUse, error) {
	var index struct {
		Harnesses []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Latest string `json:"latest"`
		} `json:"harnesses"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if os.IsNotExist(err) {
		return map[string][]HarnessUse{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	out := map[string][]HarnessUse{}
	for _, h := range index.Harnesses {
		var hi struct {
			Versions []struct {
				Version string `json:"version"`
			} `json:"versions"`
		}
		data, err := os.ReadFile(filepath.Join(dir, h.ID, "index.json"))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &hi); err != nil {
			return nil, err
		}
		versions := make([]string, 0, len(hi.Versions))
		for _, v := range hi.Versions {
			versions = append(versions, v.Version)
		}
		sort.Slice(versions, func(i, j int) bool { return refdiff.VersionCompare(versions[i], versions[j]) < 0 })
		type key struct{ model, role, label string }
		uses := map[key]*HarnessUse{}
		var order []key
		// newest is the newest build whose models were captured: a build tracked without its
		// model section (a capture that failed, a build only listed) says nothing of them.
		newest := ""
		for _, v := range versions {
			var s harnessSnapshot
			data, err := os.ReadFile(filepath.Join(dir, h.ID, "versions", v+".json"))
			if err != nil || json.Unmarshal(data, &s) != nil || s.Model == nil {
				continue
			}
			newest = v
			for _, u := range buildUses(&s) {
				k := key{norm(harnessModel(u.model)), u.role, u.label}
				if k.model == "" {
					continue
				}
				hu := uses[k]
				if hu == nil {
					hu = &HarnessUse{Harness: h.ID, Name: h.Name, Role: u.role, Label: u.label, First: v}
					uses[k] = hu
					order = append(order, k)
				}
				hu.Last = v
				hu.Builds++
				if u.params != nil {
					hu.Params = u.params
				}
			}
		}
		for _, k := range order {
			u := uses[k]
			u.Current = u.Last == newest
			out[k.model] = append(out[k.model], *u)
		}
	}
	return out, nil
}

type buildUse struct {
	model, role, label string
	params             map[string]any
}

// buildUses are the ways one build uses models.
func buildUses(s *harnessSnapshot) []buildUse {
	var out []buildUse
	add := func(m *string, role, label string, params map[string]any) {
		if m != nil && *m != "" {
			out = append(out, buildUse{*m, role, label, params})
		}
	}
	if s.Model != nil {
		add(s.Model.Default, "default", "", s.Model.Params)
		labels := make([]string, 0, len(s.Model.Aliases))
		for a := range s.Model.Aliases {
			labels = append(labels, a)
		}
		sort.Strings(labels)
		for _, a := range labels {
			m := s.Model.Aliases[a]
			add(&m, "alias", a, nil)
		}
	}
	if b := s.Behavior; b != nil {
		if b.Compaction != nil {
			add(b.Compaction.Model, "compaction", "", nil)
		}
		if b.Subagents != nil {
			add(b.Subagents.Model, "subagents", "", nil)
		}
		if b.Retries != nil {
			add(b.Retries.FallbackModel, "fallback", "", nil)
		}
	}
	var side []string
	for _, r := range s.SideRequests {
		if r.Model != nil && *r.Model != "" && !slices.Contains(side, *r.Model) {
			side = append(side, *r.Model)
			add(r.Model, "side_request", "", nil)
		}
	}
	return out
}

// harnessModel is a model id as a harness writes it, as its vendor does: without a context
// suffix (claude-opus-5-5[1m]) or a cloud's prefix (us.anthropic.claude-…, anthropic/…).
func harnessModel(m string) string {
	m = reBracket.ReplaceAllString(m, "")
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	for _, p := range []string{"us.anthropic.", "eu.anthropic.", "global.anthropic.", "anthropic.", "openai."} {
		m = strings.TrimPrefix(m, p)
	}
	return m
}

var reBracket = regexp.MustCompile(`\[[^\]]*\]$`)

// usesFor are a model's uses: those under its own id, and under an id its vendor gives it as
// well (an alias, a dated snapshot of it).
func usesFor(all map[string][]HarnessUse, s *Snapshot) []HarnessUse {
	ids := []string{norm(s.ID)}
	for _, key := range []string{factAliases, factSnapshots} {
		if l, ok := s.Facts[key].([]string); ok {
			for _, a := range l {
				ids = append(ids, norm(a))
			}
		}
	}
	// A dated id (claude-opus-4-5-20251101) is also used by its undated name.
	if d := reDateSuffix.ReplaceAllString(s.ID, ""); d != s.ID {
		ids = append(ids, norm(d))
	}
	out := []HarnessUse{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, all[id]...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Harness != out[j].Harness {
			return out[i].Harness < out[j].Harness
		}
		return roleRank(out[i].Role) < roleRank(out[j].Role)
	})
	return out
}

func roleRank(r string) int {
	return slices.Index([]string{"default", "alias", "subagents", "compaction", "fallback", "side_request"}, r)
}
