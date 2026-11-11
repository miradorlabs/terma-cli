package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// The join with the Harness Reference (docs/harnesses): which coding-agent harness builds use a
// model, and how. Each build's snapshot says the model a session starts on, the aliases it
// offers, and the models its compaction, subagents, fallback and side requests ask for. The
// reference reads the Harness Reference as its readers do, from where it is published, and
// keeps what each build uses in harness-builds.json beside the models: a run reads each
// harness's index and only the builds it has not read before, never the whole tree.

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

// harnessBuildsFile is the file, beside the models, that keeps what each build uses.
const harnessBuildsFile = "harness-builds.json"

// harnessBuilds is harness-builds.json: what each tracked harness build uses, by harness id and
// version, as the join last read it.
type harnessBuilds struct {
	SchemaVersion int                         `json:"schema_version"`
	Harnesses     map[string]*harnessOfBuilds `json:"harnesses"`
}

type harnessOfBuilds struct {
	Name   string                  `json:"name"`
	Builds map[string]*buildModels `json:"builds"`
}

// buildModels is what one build uses. A build whose models were not captured (a capture that
// failed, a build only listed) says nothing of them: Captured false.
type buildModels struct {
	// CapturedAt is when the Harness Reference last captured the build, as its index says: a build
	// captured again (a partial capture the night completes) is read again.
	CapturedAt string     `json:"captured_at,omitempty"`
	Captured   bool       `json:"captured"`
	Uses       []buildUse `json:"uses,omitempty"`
}

// harnessSource reads a file of the Harness Reference by its path below docs/harnesses: from a
// folder, or over HTTP from where it is published. A file that is not there is errNotFound.
type harnessSource func(ctx context.Context, path string) ([]byte, error)

// harnessesAt is the source at loc: a URL (https://…/docs/harnesses) or a folder.
func harnessesAt(c *client, loc string) harnessSource {
	if strings.HasPrefix(loc, "https://") || strings.HasPrefix(loc, "http://") {
		base := strings.TrimSuffix(loc, "/")
		return func(ctx context.Context, path string) ([]byte, error) {
			text, err := c.text(ctx, base+"/"+path)
			return []byte(text), err
		}
	}
	return func(_ context.Context, path string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(loc, filepath.FromSlash(path)))
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", path, errNotFound)
		}
		return data, err
	}
}

// readHarnessBuilds brings cache up to date with the Harness Reference: each harness's builds as
// its index lists them now, reading only those cache does not hold. What cannot be read this time
// (the index, a harness's index, a build) keeps what cache said: an outage is no change.
func readHarnessBuilds(ctx context.Context, src harnessSource, cache *harnessBuilds) (*harnessBuilds, []error) {
	out := &harnessBuilds{SchemaVersion: schemaVersion, Harnesses: map[string]*harnessOfBuilds{}}
	if cache == nil || cache.Harnesses == nil {
		cache = &harnessBuilds{Harnesses: map[string]*harnessOfBuilds{}}
	}
	var index struct {
		Harnesses []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"harnesses"`
	}
	data, err := src(ctx, "index.json")
	if err == nil {
		err = json.Unmarshal(data, &index)
	}
	if err != nil {
		// Not there, or not read: either way nothing new is known of the harnesses, and what was
		// read of them before stands (a missing index is a CDN's hiccup or a move, not every
		// harness gone).
		cache.SchemaVersion = schemaVersion
		if errors.Is(err, errNotFound) && len(cache.Harnesses) == 0 {
			return cache, nil
		}
		return cache, []error{err}
	}
	var errs []error
	for _, h := range index.Harnesses {
		if !validID.MatchString(h.ID) {
			continue
		}
		before := cache.Harnesses[h.ID]
		var hi struct {
			Versions []struct {
				Version    string `json:"version"`
				CapturedAt string `json:"captured_at"`
			} `json:"versions"`
		}
		data, err := src(ctx, h.ID+"/index.json")
		if err == nil {
			err = json.Unmarshal(data, &hi)
		}
		if err != nil {
			if before != nil {
				out.Harnesses[h.ID] = before
			}
			if !errors.Is(err, errNotFound) {
				errs = append(errs, err)
			}
			continue
		}
		now := &harnessOfBuilds{Name: h.Name, Builds: map[string]*buildModels{}}
		var todo []string
		capturedAt := map[string]string{}
		for _, v := range hi.Versions {
			if !validID.MatchString(v.Version) {
				continue
			}
			capturedAt[v.Version] = v.CapturedAt
			if before != nil && before.Builds[v.Version] != nil && before.Builds[v.Version].CapturedAt == v.CapturedAt {
				now.Builds[v.Version] = before.Builds[v.Version]
			} else {
				todo = append(todo, v.Version)
			}
		}
		read := make([]*buildModels, len(todo))
		readErrs := make([]error, len(todo))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i, v := range todo {
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				read[i], readErrs[i] = readBuild(ctx, src, h.ID, v)
			})
		}
		wg.Wait()
		for i, v := range todo {
			switch {
			case readErrs[i] != nil:
				// Read again next time; what was read of it before stands meanwhile.
				errs = append(errs, readErrs[i])
				if before != nil && before.Builds[v] != nil {
					now.Builds[v] = before.Builds[v]
				}
			case read[i] != nil:
				read[i].CapturedAt = capturedAt[v]
				now.Builds[v] = read[i]
			}
		}
		out.Harnesses[h.ID] = now
	}
	return out, errs
}

// readBuild is what one build's snapshot says it uses; nil for a build listed without one.
func readBuild(ctx context.Context, src harnessSource, harness, version string) (*buildModels, error) {
	data, err := src(ctx, harness+"/versions/"+version+".json")
	if errors.Is(err, errNotFound) {
		return &buildModels{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s harnessSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s %s: %w", harness, version, err)
	}
	if s.Model == nil {
		return &buildModels{}, nil
	}
	return &buildModels{Captured: true, Uses: buildUses(&s)}, nil
}

// harnessUses says, by model id as each harness names it (normalized, norm), every way a
// harness's builds use it: oldest build first, and current where the harness's newest build whose
// models were captured still does.
func harnessUses(hb *harnessBuilds) map[string][]HarnessUse {
	out := map[string][]HarnessUse{}
	ids := make([]string, 0, len(hb.Harnesses))
	for id := range hb.Harnesses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h := hb.Harnesses[id]
		versions := make([]string, 0, len(h.Builds))
		for v := range h.Builds {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool { return refdiff.VersionCompare(versions[i], versions[j]) < 0 })
		type key struct{ model, role, label string }
		uses := map[key]*HarnessUse{}
		var order []key
		newest := ""
		for _, v := range versions {
			b := h.Builds[v]
			if !b.Captured {
				continue
			}
			newest = v
			for _, u := range b.Uses {
				k := key{norm(harnessModel(u.Model)), u.Role, u.Label}
				if k.model == "" {
					continue
				}
				hu := uses[k]
				if hu == nil {
					hu = &HarnessUse{Harness: id, Name: h.Name, Role: u.Role, Label: u.Label, First: v}
					uses[k] = hu
					order = append(order, k)
				}
				hu.Last = v
				hu.Builds++
				if u.Params != nil {
					hu.Params = u.Params
				}
			}
		}
		for _, k := range order {
			u := uses[k]
			u.Current = u.Last == newest
			out[k.model] = append(out[k.model], *u)
		}
	}
	return out
}

// buildUse is one way a build uses a model.
type buildUse struct {
	Model  string         `json:"model"`
	Role   string         `json:"role"`
	Label  string         `json:"label,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// buildUses are the ways one build uses models.
func buildUses(s *harnessSnapshot) []buildUse {
	var out []buildUse
	add := func(m *string, role, label string, params map[string]any) {
		if m != nil && *m != "" {
			out = append(out, buildUse{Model: *m, Role: role, Label: label, Params: params})
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
