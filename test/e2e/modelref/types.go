package main

import (
	"time"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// The Model Reference's published files (modelref/SCHEMA.md). Field names are the schema's.

// schemaVersion is the published schema's version.
const schemaVersion = 1

// Index is docs/models/index.json: every vendor and every model, newest release first.
type Index struct {
	SchemaVersion int          `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Vendors       []VendorInfo `json:"vendors"`
	Models        []Entry      `json:"models"`
}

// VendorInfo is one vendor whose models the reference follows.
type VendorInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Docs      string `json:"docs"`
	Changelog string `json:"changelog"`
	Models    int    `json:"models"`
}

// Entry is one model as the index lists it: who makes it, when it came out, its headline
// facts, and when the reference last saw it change.
type Entry struct {
	ID              string      `json:"id"`
	Vendor          string      `json:"vendor"`
	Name            string      `json:"name"`
	Family          string      `json:"family"`
	Status          *string     `json:"status"`
	ReleasedAt      *string     `json:"released_at"`
	FirstSeen       time.Time   `json:"first_seen"`
	Predecessor     *string     `json:"predecessor"`
	Successor       *string     `json:"successor"`
	ContextWindow   *int64      `json:"context_window"`
	MaxOutput       *int64      `json:"max_output"`
	KnowledgeCutoff *string     `json:"knowledge_cutoff"`
	Reasoning       *bool       `json:"reasoning"`
	Harnesses       []string    `json:"harnesses"`
	LastChange      *LastChange `json:"last_change"`
	Probed          bool        `json:"probed"`
}

// LastChange is the newest step of a model's timeline.
type LastChange struct {
	At      time.Time `json:"at"`
	Changes int       `json:"changes"`
}

// Snapshot is docs/models/<model>/snapshot.json: everything the reference holds of a model,
// as last observed. Facts are the values the reference stands behind, each from the most
// authoritative source that states it; Provenance is every source's own value of each.
type Snapshot struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Vendor        string    `json:"vendor"`
	Name          string    `json:"name"`
	Family        string    `json:"family"`
	FirstSeen     time.Time `json:"first_seen"`
	// Listed says whether the vendor still lists the model (its docs, or its API): a model it
	// stops listing keeps its page, and its timeline says when it went.
	Listed bool `json:"listed"`

	Facts      map[string]any            `json:"facts"`
	Provenance map[string]map[string]any `json:"provenance"`
	// Disagreements are the facts whose sources state different values.
	Disagreements []string `json:"disagreements"`
	// Sources are the sources that describe the model, each with the page or endpoint read.
	Sources map[string]string `json:"sources"`

	// Docs are the vendor's own pages about the model, whole, as text.
	Docs map[string]Page `json:"docs"`
	// API is what the vendor's API says of the model: its own listing of it, and what the
	// reference's probes saw it accept, reject and count. Absent where no key was configured.
	API *API `json:"api"`
	// Harnesses are the coding-agent harness builds that use the model (the Harness Reference).
	Harnesses []HarnessUse `json:"harnesses"`
}

// Page is one of the vendor's pages about a model, as Markdown.
type Page struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
}

// API is what the vendor's API said of a model.
type API struct {
	// Listing is the model's object in the vendor's own model list (GET /v1/models and its
	// kin), as the vendor returns it, without prices.
	Listing map[string]any `json:"listing,omitempty"`
	Probes  *Probes        `json:"probes,omitempty"`
}

// Probes are the reference's own requests to a model: which request parameters it accepts,
// how many tokens it counts in a fixed corpus, and the shape of its responses.
type Probes struct {
	// At is when the probes first saw what they show: a run that sees the same keeps it.
	At       time.Time `json:"at"`
	Endpoint string    `json:"endpoint"`
	// Unavailable says why the plain request was refused, where it was (a model the reference's
	// key may not use): no case was sent then.
	Unavailable string `json:"unavailable,omitempty"`
	// Params is each probe case's outcome, by its name.
	Params map[string]ProbeResult `json:"params"`
	// Tokens is the token count of each text of the corpus (probecorpus.go), by its name.
	Tokens map[string]int `json:"tokens,omitempty"`
	// TokenizerLike are the other models that count every text of the corpus the same.
	TokenizerLike []string       `json:"tokenizer_like,omitempty"`
	Response      *ResponseShape `json:"response,omitempty"`

	// Errors are what this run's probes could not observe (a case the API did not answer, the
	// corpus not counted), for the run's log: never published, as they are the run's and not the
	// model's.
	Errors []string `json:"-"`
	// unanswered are the cases the API did not answer this run, and streamMissed says the stream
	// was not read: what they said before stands (keepUnobserved).
	unanswered   map[string]bool
	streamMissed bool
}

// ProbeResult is what the API did with one probe request.
type ProbeResult struct {
	Accepted bool   `json:"accepted"`
	Status   int    `json:"status"`
	Error    string `json:"error,omitempty"`
}

// ResponseShape is what a plain answer from the model carries: the keys of its usage, the model
// it said answered, the response headers it sent (names only), and the event types its stream
// sent, in order. Why one short answer stopped, and a backend's fingerprint, are the run's, and
// not kept.
type ResponseShape struct {
	Model        string   `json:"model,omitempty"`
	UsageKeys    []string `json:"usage_keys,omitempty"`
	Headers      []string `json:"headers,omitempty"`
	StreamEvents []string `json:"stream_events,omitempty"`
}

// HarnessUse is one way a harness uses a model, over the builds that do.
type HarnessUse struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	// Role is how: "default" (the model a session starts on), "alias" (Label names it),
	// "compaction", "subagents", "fallback", "side_request".
	Role  string `json:"role"`
	Label string `json:"label,omitempty"`
	// First and Last are the oldest and newest tracked builds that use it so, Builds how many.
	First  string `json:"first"`
	Last   string `json:"last"`
	Builds int    `json:"builds"`
	// Current says the harness's newest tracked build still does.
	Current bool `json:"current"`
	// Params are the request parameters the newest such build sends with it (default role).
	Params map[string]any `json:"params,omitempty"`
}

// Timeline is docs/models/<model>/timeline.json: what changed in what the reference observes of
// a model, newest step first.
type Timeline struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Steps         []Step `json:"steps"`
	// Archives are the files the older steps are in (timeline-1.json, the oldest, on), as the
	// Harness Reference keeps its own.
	Archives []TimelineArchive `json:"archives,omitempty"`
}

// TimelineArchive is a file of older timeline steps: its name, the times of its oldest and
// newest step, and how many it holds.
type TimelineArchive struct {
	File  string    `json:"file"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	Steps int       `json:"steps"`
}

// Step is one run's changes to a model.
type Step struct {
	At      time.Time        `json:"at"`
	Event   string           `json:"event"` // "listed" | "changed" | "unlisted"
	Changes []refdiff.Change `json:"changes"`
}

// Notes is docs/models/<model>/notes.json: a model's release notes. What its vendor says is new
// in it, its vendor's changelog entries that name it, and what the reference sees differ from
// the model before it in its family.
type Notes struct {
	SchemaVersion int     `json:"schema_version"`
	ID            string  `json:"id"`
	Predecessor   *string `json:"predecessor"`
	WhatsNew      *Page   `json:"whats_new"`
	// VendorHighlights are what the vendor says is new, in its words: its what's new page's
	// headings, or its changelog's launch entries' first sentences.
	VendorHighlights []string         `json:"vendor_highlights"`
	Changelog        []ChangelogEntry `json:"changelog"`
	Changes          []refdiff.Change `json:"changes"`
	// Highlights are the changes from the model before most worth a headline, as the reference
	// observes them.
	Highlights []string `json:"highlights"`
}

// ChangelogEntry is one entry of a vendor's changelog that names a model.
type ChangelogEntry struct {
	Date string `json:"date"`
	URL  string `json:"url"`
	Text string `json:"text"`
}

// Recent is docs/models/recent.json: the newest events across every model, for a feed.
type Recent struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   time.Time     `json:"generated_at"`
	Events        []RecentEvent `json:"events"`
}

// RecentEvent is a model listed, changed or unlisted.
type RecentEvent struct {
	At         time.Time `json:"at"`
	Model      string    `json:"model"`
	Vendor     string    `json:"vendor"`
	Name       string    `json:"name"`
	Event      string    `json:"event"`
	Changes    int       `json:"changes"`
	Highlights []string  `json:"highlights"`
}
