package main

import (
	"time"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// The published Harness Reference, as reference/SCHEMA.md specifies it: an index of every
// harness, each harness's versions, each version's snapshot, and the diff from each version
// to the next. Field names here are the schema's; a change to one is a schema change.

const schemaVersion = refdiff.SchemaVersion

// The snapshot and diff types are refdiff's.
type (
	Snapshot      = refdiff.Snapshot
	Capture       = refdiff.Capture
	Diff          = refdiff.Diff
	DiffSource    = refdiff.DiffSource
	ChangelogNote = refdiff.ChangelogNote
	DiffSummary   = refdiff.DiffSummary
	Change        = refdiff.Change
	TextStats     = refdiff.TextStats
	TermaEffect   = refdiff.TermaEffect
	SourceNote    = refdiff.SourceNote
	SourceRef     = refdiff.SourceRef
)

var sections = refdiff.Sections

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
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
	// ReleasedAt is when To was published to its registry, nil where that is not known: At is
	// when it was captured.
	ReleasedAt *time.Time     `json:"released_at"`
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
	// Signing is Gatekeeper's verdict on the build's signature when last checked: "valid",
	// "revoked", "rejected" or "unsigned"; nil where it was not checked (off macOS, a script).
	Signing *string `json:"signing"`
}

// Changelogs is docs/harnesses/<harness>/changelog.json: what the vendor's changelog says of
// each version, where it says something that could be read, so a comparison of any two
// versions can judge what the changelogs between them mention.
type Changelogs struct {
	SchemaVersion int                      `json:"schema_version"`
	Harness       string                   `json:"harness"`
	Versions      map[string]ChangelogNote `json:"versions"`
}

// Timeline is docs/harnesses/<harness>/timeline.json: the newest steps between consecutive
// versions, and the archives that hold the older ones; an archive (timeline-<n>.json) is a
// Timeline of its own steps, without archives.
type Timeline struct {
	SchemaVersion int            `json:"schema_version"`
	Harness       string         `json:"harness"`
	Steps         []TimelineStep `json:"steps"`
	// Archives are the files of the steps older than Steps, oldest first; absent where Steps
	// holds every step.
	Archives []TimelineArchive `json:"archives,omitempty"`
}

// TimelineArchive is one file of older steps: its name beside timeline.json, the versions of
// its oldest and newest step, and how many steps it holds.
type TimelineArchive struct {
	File  string `json:"file"`
	First string `json:"first"`
	Last  string `json:"last"`
	Steps int    `json:"steps"`
}

// TimelineStep is one version's changes from the one before it.
type TimelineStep struct {
	Version  string           `json:"version"`
	Previous string           `json:"previous"`
	Changes  []TimelineChange `json:"changes"`
}

// TimelineChange is a change as a timeline lists it: what it is, not its values.
type TimelineChange struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Op         string `json:"op"`
	Title      string `json:"title"`
	Impact     string `json:"impact"`
	Documented *bool  `json:"documented"`
}

// Recent is docs/harnesses/recent.json: the newest versions across every harness, each with
// its change summary, newest release first, so a feed needs one request.
type Recent struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   time.Time     `json:"generated_at"`
	Versions      []RecentEntry `json:"versions"`
}

// RecentEntry is one version in the feed: its harness, and its entry in the harness's index.
type RecentEntry struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	VersionEntry
}
