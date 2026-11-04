package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// What a harness's vendor documents of its configuration, beside what the build reads. A build
// is the truth of what it reads; the vendor's reference says what each setting means, its type,
// its default and where it goes, and what it leaves out is news: a setting the build reads that
// no page mentions. A reference describes the vendor's current release, not every build, so it
// is fetched each night and kept with that night's newest build (Snapshot.Docs), never compared
// as a change of the build: a page's edit is no release.

// docSources are the pages a harness's configuration is documented on, as markdown.
var docSources = map[string]struct{ settings, env string }{
	"claude": {settings: "https://code.claude.com/docs/en/settings-reference.md", env: "https://code.claude.com/docs/en/env-vars.md"},
}

// docSetting is one setting as its reference describes it.
type docSetting struct {
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Default     string `json:"default,omitempty"`
	Scope       string `json:"scope,omitempty"`
	ScopeNote   string `json:"scope_note,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Read        bool   `json:"read"` // the build reads it
}

var (
	docHeading = regexp.MustCompile("^#{3,4} `([^`]+)`\\s*$")
	docTopic   = regexp.MustCompile(`^## (.+?)\s*$`)
	docBullet  = regexp.MustCompile(`^\* \*\*([^*]+)\*\*:\s*(.*)$`)
	docEnvRow  = regexp.MustCompile("^\\| `([A-Z][A-Z0-9_]+)` \\| (.*) \\|\\s*$")
	mdLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// A scope is a link to the scopes table, or a name in backticks before any note.
	docScopes = regexp.MustCompile("\\[`([^`]+)`\\]\\(#scopes\\)")
)

// plainText is markdown as text: links as their words.
func plainText(s string) string { return strings.TrimSpace(mdLink.ReplaceAllString(s, "$1")) }

// parseSettingsDoc reads a settings reference: each setting under its heading, its first
// paragraph its description, its bullets its scope, type and default.
func parseSettingsDoc(md string) map[string]*docSetting {
	out := map[string]*docSetting{}
	var cur *docSetting
	topic := ""
	inFence, inDesc := false, false
	for line := range strings.SplitSeq(md, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := docTopic.FindStringSubmatch(line); m != nil {
			topic, cur = m[1], nil
			continue
		}
		if m := docHeading.FindStringSubmatch(line); m != nil {
			cur = &docSetting{Topic: topic}
			out[m[1]] = cur
			inDesc = true
			continue
		}
		if cur == nil {
			continue
		}
		if m := docBullet.FindStringSubmatch(line); m != nil {
			inDesc = false
			switch m[1] {
			case "Scope":
				// "[`Any file`](#scopes). For a self-hosted …": the scopes are the names in
				// backticks; what follows is a note.
				if names := docScopes.FindAllStringSubmatch(m[2], -1); len(names) > 0 {
					var ss []string
					for _, n := range names {
						ss = append(ss, n[1])
					}
					cur.Scope = strings.Join(ss, ", ")
					if _, note, ok := strings.Cut(plainText(m[2]), "`. "); ok {
						cur.ScopeNote = strings.TrimSpace(note)
					}
				} else {
					head, note, _ := strings.Cut(plainText(m[2]), ". ")
					cur.Scope, cur.ScopeNote = strings.Trim(strings.TrimSpace(head), "`"), strings.TrimSpace(note)
				}
			case "Type":
				cur.Type = plainText(m[2])
			case "Default":
				cur.Default = plainText(m[2])
			}
			continue
		}
		switch t := strings.TrimSpace(line); {
		case t == "" && cur.Description != "":
			inDesc = false
		case inDesc && t != "":
			cur.Description = strings.TrimSpace(cur.Description + " " + plainText(t))
		}
	}
	return out
}

// parseEnvDoc reads an environment variable reference's table: each variable and what it does.
func parseEnvDoc(md string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(md, "\n") {
		if m := docEnvRow.FindStringSubmatch(line); m != nil {
			out[m[1]] = plainText(m[2])
		}
	}
	return out
}

// vendorDocs are a harness's references, read once a run.
type vendorDocs struct {
	settings  map[string]*docSetting
	env       map[string]string
	source    []string
	fetchedAt time.Time
}

// fetchDocs reads harness's references, nil where it has none or none could be read.
func fetchDocs(harness string, now time.Time) *vendorDocs {
	src, ok := docSources[harness]
	if !ok {
		return nil
	}
	d := &vendorDocs{fetchedAt: now}
	if data, err := fetcher(src.settings); err == nil {
		d.settings = parseSettingsDoc(string(data))
		d.source = append(d.source, src.settings)
	}
	if data, err := fetcher(src.env); err == nil {
		d.env = parseEnvDoc(string(data))
		d.source = append(d.source, src.env)
	}
	if len(d.settings) == 0 && len(d.env) == 0 {
		return nil
	}
	return d
}

// globalScope is a setting kept outside the settings files (Claude Code's ~/.claude.json),
// which a settings schema does not hold: not reading it is no sign of not knowing it.
func globalScope(scope string) bool { return strings.Contains(strings.ToLower(scope), "global") }

// docsOf is what the references say of s's build: each documented setting and variable, and
// whether the build reads it; the settings and variables it reads that no reference mentions;
// those documented that it does not read.
func docsOf(s *Snapshot, d *vendorDocs) map[string]any {
	opts, _ := s.Config["options"].(map[string]any)
	readTop := map[string]bool{}
	for k := range opts {
		top, _, _ := strings.Cut(k, ".")
		top, _, _ = strings.Cut(top, "[")
		readTop[top] = true
		readTop[k] = true
	}
	envRead := map[string]bool{}
	if vs, ok := s.Config["env_vars"].([]any); ok {
		for _, v := range vs {
			if name, ok := v.(string); ok {
				envRead[name] = true
			}
		}
	}
	out := map[string]any{"source": d.source, "fetched_at": d.fetchedAt}
	if len(d.settings) > 0 && len(opts) > 0 {
		settings := map[string]any{}
		documentedTop := map[string]bool{}
		var notRead []string
		for k, ds := range d.settings {
			ds.Read = readTop[k]
			settings[k] = jsonOf(ds)
			top, _, _ := strings.Cut(k, ".")
			documentedTop[top] = true
			if !ds.Read && !globalScope(ds.Scope) {
				notRead = append(notRead, k)
			}
		}
		var undocumented []string
		for k := range readTop {
			if !strings.ContainsAny(k, ".[") && !documentedTop[k] && k != "$schema" {
				undocumented = append(undocumented, k)
			}
		}
		sort.Strings(notRead)
		sort.Strings(undocumented)
		out["settings"] = settings
		out["undocumented_settings"] = nonNil(undocumented)
		out["documented_not_read"] = nonNil(notRead)
	}
	if len(d.env) > 0 && len(envRead) > 0 {
		env := map[string]any{}
		var notRead, undocumented []string
		for name, desc := range d.env {
			env[name] = map[string]any{"description": desc, "read": envRead[name]}
			if !envRead[name] {
				notRead = append(notRead, name)
			}
		}
		for name := range envRead {
			if _, ok := d.env[name]; !ok {
				undocumented = append(undocumented, name)
			}
		}
		sort.Strings(notRead)
		sort.Strings(undocumented)
		out["env_vars"] = env
		out["undocumented_env_vars"] = nonNil(undocumented)
		out["documented_env_not_read"] = nonNil(notRead)
	}
	return out
}
