package main

import (
	"encoding/json"
	"time"
)

// The published Harness Reference, as reference/SCHEMA.md specifies it: an index of every
// harness, each harness's versions, each version's snapshot, and the diff from each version
// to the next. Field names here are the schema's; a change to one is a schema change.

const schemaVersion = 1

// Index is docs/harnesses/index.json.
type Index struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Harnesses     []HarnessEntry `json:"harnesses"`
}

// HarnessEntry is one harness as the index lists it.
type HarnessEntry struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Vendor          string      `json:"vendor"`
	Homepage        string      `json:"homepage,omitempty"`
	Source          *SourceInfo `json:"source"`
	Changelog       string      `json:"changelog,omitempty"`
	Latest          string      `json:"latest"`
	Support         string      `json:"support"`
	VersionsTracked int         `json:"versions_tracked"`
	FirstTracked    string      `json:"first_tracked,omitempty"`
	LastChange      *LastChange `json:"last_change"`
}

// SourceInfo is where a harness's source is public.
type SourceInfo struct {
	Repo     string `json:"repo"`
	URL      string `json:"url"`
	Language string `json:"language"`
}

// LastChange is the newest diff of a harness.
type LastChange struct {
	From       string         `json:"from"`
	To         string         `json:"to"`
	At         time.Time      `json:"at"`
	Changes    int            `json:"changes"`
	Categories map[string]int `json:"categories"`
}

// HarnessIndex is docs/harnesses/<harness>/index.json.
type HarnessIndex struct {
	SchemaVersion int            `json:"schema_version"`
	Harness       HarnessEntry   `json:"harness"`
	Versions      []VersionEntry `json:"versions"`
}

// VersionEntry is one version as its harness's index lists it.
type VersionEntry struct {
	Version      string         `json:"version"`
	ReleasedAt   *time.Time     `json:"released_at"`
	CapturedAt   time.Time      `json:"captured_at"`
	Platforms    []string       `json:"platforms"`
	Support      string         `json:"support"`
	Previous     *string        `json:"previous"`
	Changes      int            `json:"changes"`
	Categories   map[string]int `json:"categories"`
	Impact       map[string]int `json:"impact"`
	Undocumented *int           `json:"undocumented"`
	Highlights   []string       `json:"highlights"`
}

// Snapshot is docs/harnesses/<harness>/versions/<version>.json. Each section is what one kind
// of capture saw; nil is not observed. Sections are generic JSON beneath their top level so
// that a new capture adds a key, not a type.
type Snapshot struct {
	SchemaVersion int       `json:"schema_version"`
	Harness       string    `json:"harness"`
	Version       string    `json:"version"`
	Platform      string    `json:"platform"`
	CapturedAt    time.Time `json:"captured_at"`
	Capture       Capture   `json:"capture"`

	Model        map[string]any `json:"model,omitempty"`
	Prompt       map[string]any `json:"prompt,omitempty"`
	Tools        []any          `json:"tools,omitempty"`
	Caching      map[string]any `json:"caching,omitempty"`
	Request      map[string]any `json:"request,omitempty"`
	SideRequests []any          `json:"side_requests,omitempty"`
	Behavior     map[string]any `json:"behavior,omitempty"`
	Telemetry    map[string]any `json:"telemetry,omitempty"`
	Hooks        map[string]any `json:"hooks,omitempty"`
	CLI          map[string]any `json:"cli,omitempty"`
	Config       map[string]any `json:"config,omitempty"`
	Network      map[string]any `json:"network,omitempty"`
	Storage      map[string]any `json:"storage,omitempty"`
	Binary       map[string]any `json:"binary,omitempty"`
	Support      map[string]any `json:"support,omitempty"`
}

// Capture says how a snapshot was taken.
type Capture struct {
	Command []string `json:"command,omitempty"`
	Partial bool     `json:"partial"`
	Notes   []string `json:"notes,omitempty"`
	// Sections says when each section was last captured: a night that did not capture one
	// keeps the last night's.
	Sections map[string]time.Time `json:"sections,omitempty"`
}

// sections are a snapshot's sections in the schema's order, which is also the order a diff's
// categories are listed in.
var sections = []string{"model", "prompt", "tools", "caching", "request", "side_requests", "behavior",
	"telemetry", "hooks", "cli", "config", "network", "storage", "binary", "support"}

// tree is s as generic JSON, by section.
func (s *Snapshot) tree() map[string]any {
	data, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

// Diff is docs/harnesses/<harness>/diffs/<from>..<to>.json.
type Diff struct {
	SchemaVersion int            `json:"schema_version"`
	Harness       string         `json:"harness"`
	From          string         `json:"from"`
	To            string         `json:"to"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Source        *DiffSource    `json:"source"`
	Changelog     *ChangelogNote `json:"changelog"`
	Summary       DiffSummary    `json:"summary"`
	Changes       []Change       `json:"changes"`
}

// DiffSource links the source's changes between the two builds.
type DiffSource struct {
	Compare string `json:"compare"`
}

// ChangelogNote is what the vendor's changelog says of the newer build.
type ChangelogNote struct {
	URL  string `json:"url"`
	Text string `json:"text"`
}

// DiffSummary counts a diff's changes.
type DiffSummary struct {
	Changes      int            `json:"changes"`
	Categories   map[string]int `json:"categories"`
	Impact       map[string]int `json:"impact"`
	Undocumented *int           `json:"undocumented"`
}

// Change is one difference between two snapshots.
type Change struct {
	ID       string       `json:"id"`
	Category string       `json:"category"`
	Op       string       `json:"op"` // added | removed | changed
	Path     []string     `json:"path"`
	Title    string       `json:"title"`
	Impact   string       `json:"impact"` // breaking | degrading | notable | informational
	Before   any          `json:"before"`
	After    any          `json:"after"`
	TextDiff *string      `json:"text_diff"`
	Stats    *TextStats   `json:"stats,omitempty"`
	Terma    *TermaEffect `json:"terma"`
	Source   *SourceNote  `json:"source"`
	// Documented is whether the vendor's changelog for the newer build mentions it; nil
	// without a changelog.
	Documented *bool `json:"documented"`
}

// TextStats counts what a text change added and removed.
type TextStats struct {
	AddedChars   int `json:"added_chars"`
	RemovedChars int `json:"removed_chars"`
}

// TermaEffect names the capabilities whose result changed with a change.
type TermaEffect struct {
	Capabilities []string `json:"capabilities"`
}

// SourceNote is where the harness's public source names what changed.
type SourceNote struct {
	Refs []SourceRef `json:"refs"`
	Gone bool        `json:"gone"`
}

// SourceRef is a line of a harness's source.
type SourceRef struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	URL  string `json:"url"`
}
