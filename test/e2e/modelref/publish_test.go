package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// testRun is a run that read every source its observations name.
func testRun(now time.Time, obs ...observation) *run {
	r := &run{now: now, read: map[string]bool{}, failed: map[string]string{}, keyed: map[string]bool{"anthropic": true}}
	for _, o := range obs {
		r.read[o.source] = true
		r.obs = append(r.obs, o)
	}
	return r
}

func docsObs(id string, facts map[string]any) observation {
	return observation{source: "anthropic-docs", vendor: "anthropic", id: id, listed: true, facts: maps.Clone(facts),
		url: "https://platform.claude.com/docs/en/models/" + id, docs: map[string]Page{"overview": {Title: "Model page", Text: "# " + id + "\n"}}}
}

func catalogObs(id string, facts map[string]any) observation {
	return observation{source: "models.dev", vendor: "anthropic", id: id, url: "https://models.dev", facts: maps.Clone(facts)}
}

var (
	fable51 = map[string]any{factName: "Claude Fable 5.1", factReleasedAt: "2026-09-01", factContextWindow: int64(1_000_000),
		factMaxOutput: int64(128_000), factDefaultEffort: "high", factReasoningEfforts: []string{"low", "medium", "high"}}
	fable52 = map[string]any{factName: "Claude Fable 5.2", factReleasedAt: "2026-11-09", factContextWindow: int64(1_000_000),
		factMaxOutput: int64(128_000), factDefaultEffort: "medium", factLatency: "moderate", factReasoningEfforts: []string{"low", "medium", "high", "max"}}
)

func mustPublish(t *testing.T, dir string, r *run) *result {
	t.Helper()
	prev, err := readPublished(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := publish(dir, r, prev, nil, map[string][]ChangelogEntry{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func tree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		files[strings.TrimPrefix(p, dir)] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func readTimeline(t *testing.T, dir, id string) Timeline {
	t.Helper()
	var tl Timeline
	if err := readOptional(filepath.Join(dir, id, "timeline.json"), &tl); err != nil {
		t.Fatal(err)
	}
	return tl
}

// A run that finds what the run before found writes the same bytes: nothing to commit every
// quarter hour.
func TestPublishIsStable(t *testing.T) {
	dir := t.TempDir()
	obs := []observation{docsObs("claude-fable-5-1", fable51), docsObs("claude-fable-5-2", fable52),
		catalogObs("claude-fable-5-2", map[string]any{factKnowledgeCutoff: "2026-06"})}
	mustPublish(t, dir, testRun(testNow, obs...))
	before := tree(t, dir)
	mustPublish(t, dir, testRun(testNow.Add(15*time.Minute), obs...))
	after := tree(t, dir)
	for name, data := range after {
		if !bytes.Equal(before[name], data) {
			t.Errorf("%s changed on a run that found nothing new:\n%s", name, data)
		}
	}
	if len(after) != len(before) {
		t.Errorf("%d files, then %d", len(before), len(after))
	}
}

func TestTimelineRecordsAChange(t *testing.T) {
	dir := t.TempDir()
	mustPublish(t, dir, testRun(testNow, docsObs("claude-fable-5-2", fable52)))
	lower := maps.Clone(fable52)
	lower[factMaxOutput] = int64(64_000)
	mustPublish(t, dir, testRun(testNow.Add(time.Hour), docsObs("claude-fable-5-2", lower)))
	tl := readTimeline(t, dir, "claude-fable-5-2")
	if len(tl.Steps) != 2 || tl.Steps[0].Event != "changed" || tl.Steps[1].Event != "listed" {
		t.Fatalf("steps = %+v", tl.Steps)
	}
	c := tl.Steps[0].Changes
	if len(c) != 1 || c[0].Title != "Max output: 128K → 64K" || c[0].Impact != "breaking" {
		t.Errorf("changes = %+v", c)
	}
	var recent Recent
	_ = readOptional(filepath.Join(dir, "recent.json"), &recent)
	if len(recent.Events) != 2 || recent.Events[0].Event != "changed" || !recent.GeneratedAt.Equal(testNow.Add(time.Hour)) {
		t.Errorf("recent = %+v", recent)
	}
}

// A source that was not read this time (an outage) keeps saying what it said: no fact goes away
// and no model is unlisted for it.
func TestAnOutageIsNoChange(t *testing.T) {
	dir := t.TempDir()
	mustPublish(t, dir, testRun(testNow, docsObs("claude-fable-5-2", fable52),
		catalogObs("claude-fable-5-2", map[string]any{factKnowledgeCutoff: "2026-06", factOpenWeights: false})))
	r := testRun(testNow.Add(time.Hour))
	r.failed["anthropic-docs"], r.failed["models.dev"] = "503", "timeout"
	res := mustPublish(t, dir, r)
	s := res.snaps["claude-fable-5-2"]
	if s == nil || !s.Listed {
		t.Fatalf("model unlisted by an outage: %+v", s)
	}
	if s.Facts[factKnowledgeCutoff] != "2026-06" || s.Facts[factMaxOutput] != int64(128_000) {
		t.Errorf("facts lost in an outage: %v", s.Facts)
	}
	if tl := readTimeline(t, dir, "claude-fable-5-2"); len(tl.Steps) != 1 {
		t.Errorf("an outage made a step: %+v", tl.Steps[0])
	}
}

func TestUnlistedWhenTheVendorStopsListing(t *testing.T) {
	dir := t.TempDir()
	mustPublish(t, dir, testRun(testNow, docsObs("claude-fable-5-1", fable51), docsObs("claude-fable-5-2", fable52)))
	mustPublish(t, dir, testRun(testNow.Add(time.Hour), docsObs("claude-fable-5-2", fable52)))
	tl := readTimeline(t, dir, "claude-fable-5-1")
	if len(tl.Steps) != 2 || tl.Steps[0].Event != "unlisted" {
		t.Fatalf("steps = %+v", tl.Steps)
	}
	var idx Index
	_ = readOptional(filepath.Join(dir, "index.json"), &idx)
	for _, e := range idx.Models {
		if e.ID == "claude-fable-5-1" && (e.Status == nil || *e.Status != "unlisted") {
			t.Errorf("status = %v", e.Status)
		}
	}
}

// A fact one model's sources state and the other's do not is unknown there, not news.
func TestNotesCompareWhatBothState(t *testing.T) {
	dir := t.TempDir()
	res := mustPublish(t, dir, testRun(testNow, docsObs("claude-fable-5-1", fable51), docsObs("claude-fable-5-2", fable52)))
	n := res.notes["claude-fable-5-2"]
	if n.Predecessor == nil || *n.Predecessor != "claude-fable-5-1" {
		t.Fatalf("predecessor = %v", n.Predecessor)
	}
	var titles []string
	for _, c := range n.Changes {
		titles = append(titles, c.Title)
	}
	want := []string{"Default effort: high → medium", "Reasoning effort added: max"}
	if !slices.Equal(titles, want) {
		t.Errorf("changes = %q, want %q", titles, want)
	}
}

func TestAnnouncements(t *testing.T) {
	dir := t.TempDir()
	r := testRun(testNow, docsObs("claude-fable-5-1", fable51), docsObs("claude-fable-5-2", fable52))
	res := mustPublish(t, dir, r)
	if a := announcements(res, "https://terma.ai", ""); len(a) != 0 {
		t.Errorf("a vendor's first publish announced %d models", len(a))
	}
	if a := announcements(res, "https://terma.ai", "2026-11-01"); len(a) != 1 || a[0].Model != "claude-fable-5-2" {
		t.Errorf("-announce-since: %+v", a)
	}
	next := maps.Clone(fable52)
	next[factName], next[factReleasedAt] = "Claude Fable 5.3", "2026-12-01"
	long := docsObs("claude-fable-5-3", next)
	long.docs["whats_new"] = Page{Text: "## Breaking changes\n\n### " + strings.Repeat("A very long breaking change heading ", 6) +
		"\n\n### Thinking is always on\n\n### Forced tool use is refused\n"}
	res = mustPublish(t, dir, testRun(testNow.Add(time.Hour), docsObs("claude-fable-5-1", fable51), docsObs("claude-fable-5-2", fable52), long))
	a := announcements(res, "https://terma.ai", "")
	if len(a) != 1 || a[0].Model != "claude-fable-5-3" || a[0].URL != "https://terma.ai/m/claude-fable-5-3" {
		t.Fatalf("announcements = %+v", a)
	}
	text := a[0].Text
	if n := utf8.RuneCountInString(strings.TrimSuffix(text, a[0].URL)) + 23; n > 280 {
		t.Errorf("post is %d characters as X counts them:\n%s", n, text)
	}
	if !strings.HasPrefix(text, "Claude Fable 5.3 is out: 1M context, 128K output.\nNew since Claude Fable 5.2:") {
		t.Errorf("post:\n%s", text)
	}
	if got := slackEscape("Use <tool> & `computer_20251124`"); got != "Use &lt;tool&gt; &amp; `computer_20251124`" {
		t.Errorf("slackEscape = %q", got)
	}
	data, _ := json.Marshal(a[0].Slack)
	if !strings.Contains(string(data), `"View the notes"`) || !strings.Contains(string(data), "Thinking is always on") {
		t.Errorf("slack = %s", data)
	}
}

func TestFamilies(t *testing.T) {
	for id, want := range map[string]string{"claude-fable-5-2": "claude-fable", "claude-3-7-sonnet-20250219": "claude-sonnet",
		"gpt-6.1-sol": "gpt-sol", "gpt-5.5-pro": "gpt-pro", "gemini-3.5-flash-lite": "gemini-flash-lite",
		"gemini-3-flash-preview": "gemini-flash", "grok-4.20-0309-reasoning": "grok-reasoning", "grok-4.7": "grok"} {
		if got := family(id); got != want {
			t.Errorf("family(%s) = %s, want %s", id, got, want)
		}
	}
	snaps := map[string]*Snapshot{}
	for _, id := range []string{"claude-opus-4-1-20250805", "claude-opus-4-5-20251101", "claude-opus-4-6", "claude-opus-5", "claude-opus-5-5", "claude-mythos-preview"} {
		snaps[id] = &Snapshot{ID: id, Vendor: "anthropic", Family: family(id), Facts: map[string]any{}}
	}
	pred, succ := lineage(snaps)
	for id, want := range map[string]string{"claude-opus-4-5-20251101": "claude-opus-4-1-20250805", "claude-opus-4-6": "claude-opus-4-5-20251101",
		"claude-opus-5": "claude-opus-4-6", "claude-opus-5-5": "claude-opus-5"} {
		if pred[id] != want {
			t.Errorf("predecessor of %s = %q, want %s", id, pred[id], want)
		}
	}
	if _, ok := pred["claude-mythos-preview"]; ok || succ["claude-opus-5-5"] != "" {
		t.Errorf("pred %v succ %v", pred, succ)
	}
}

func TestDisagreements(t *testing.T) {
	for _, tc := range []struct {
		vals map[string]any
		want bool
	}{
		{map[string]any{"a": int64(1_000_000), "b": int64(1_048_576)}, false},
		{map[string]any{"a": int64(65_536), "b": int64(1_048_576)}, true},
		{map[string]any{"a": "2026-06", "b": "2026-06-30"}, false},
		{map[string]any{"a": "2026-04-20", "b": "2026-04-30"}, true},
		{map[string]any{"a": []string{"text"}, "b": []string{"text", "image"}}, false},
		{map[string]any{"a": true, "b": false}, true},
	} {
		if got := disagree(tc.vals); got != tc.want {
			t.Errorf("disagree(%v) = %v", tc.vals, got)
		}
	}
}

// A long timeline keeps its newest steps in timeline.json and the older ones in archives counted
// from the oldest, which a later step never rewrites; reading them back gives every step.
func TestTimelineIsPaged(t *testing.T) {
	dir := t.TempDir()
	tl := Timeline{SchemaVersion: schemaVersion, ID: "m"}
	for i := range 60 {
		tl.Steps = append([]Step{{At: testNow.Add(time.Duration(i) * time.Hour), Event: "changed", Changes: []refdiff.Change{}}}, tl.Steps...)
	}
	if err := writeTimelineFiles(dir, tl, 25); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "timeline-1.json"))
	var head Timeline
	_ = readOptional(filepath.Join(dir, "timeline.json"), &head)
	if len(head.Steps) != 25 || len(head.Archives) != 2 || head.Archives[0].Steps != 25 || head.Archives[1].Steps != 10 {
		t.Fatalf("head %d steps, archives %+v", len(head.Steps), head.Archives)
	}
	tl.Steps = append([]Step{{At: testNow.Add(100 * time.Hour), Event: "changed", Changes: []refdiff.Change{}}}, tl.Steps...)
	if err := writeTimelineFiles(dir, tl, 25); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "timeline-1.json")); !bytes.Equal(first, again) {
		t.Error("a new step rewrote the oldest archive")
	}
	all, err := readTimelineFiles(dir)
	if err != nil || len(all.Steps) != 61 || !all.Steps[0].At.Equal(testNow.Add(100*time.Hour)) || !all.Steps[60].At.Equal(testNow) {
		t.Errorf("read back %d steps (%v)", len(all.Steps), err)
	}
}

// A run that only finds the vendor's page text edited records it on the model's timeline, and
// keeps it out of the feed.
func TestPageEditsStayOutOfTheFeed(t *testing.T) {
	dir := t.TempDir()
	mustPublish(t, dir, testRun(testNow, docsObs("claude-fable-5-2", fable52)))
	edited := docsObs("claude-fable-5-2", fable52)
	edited.docs["overview"] = Page{Title: "Model page", Text: "# claude-fable-5-2\n\nA new footer.\n"}
	mustPublish(t, dir, testRun(testNow.Add(time.Hour), edited))
	if tl := readTimeline(t, dir, "claude-fable-5-2"); len(tl.Steps) != 2 || tl.Steps[0].Changes[0].Category != "docs" {
		t.Fatalf("steps = %+v", tl.Steps)
	}
	var recent Recent
	_ = readOptional(filepath.Join(dir, "recent.json"), &recent)
	if len(recent.Events) != 1 || recent.Events[0].Event != "listed" {
		t.Errorf("feed = %+v", recent.Events)
	}
}

// A model two of its vendor's lists name stays listed while one of them could not be read, even
// where the other, read, no longer names it; it is unlisted once both were read without it.
func TestUnlistedOnlyWhenEveryListSaysSo(t *testing.T) {
	dir := t.TempDir()
	api := observation{source: "anthropic-api", vendor: "anthropic", id: "claude-fable-5-1", listed: true, facts: map[string]any{}}
	mustPublish(t, dir, testRun(testNow, docsObs("claude-fable-5-1", fable51), api, docsObs("claude-fable-5-2", fable52)))
	// The docs no longer list it; the API was not read (it timed out).
	r := testRun(testNow.Add(time.Hour), docsObs("claude-fable-5-2", fable52))
	r.failed["anthropic-api"] = "timeout"
	if res := mustPublish(t, dir, r); !res.snaps["claude-fable-5-1"].Listed {
		t.Fatal("unlisted while the API, which listed it, was not read")
	}
	// Both read, neither lists it.
	r = testRun(testNow.Add(2*time.Hour), docsObs("claude-fable-5-2", fable52), observation{source: "anthropic-api", vendor: "anthropic", id: "claude-fable-5-2", listed: true, facts: map[string]any{}})
	if res := mustPublish(t, dir, r); res.snaps["claude-fable-5-1"].Listed {
		t.Error("still listed once every list was read without it")
	}
}

// An API whose key was taken away is no list: a model its docs drop is unlisted, though the API,
// read before, named it.
func TestAKeyRemovedIsNoOutage(t *testing.T) {
	dir := t.TempDir()
	api := observation{source: "anthropic-api", vendor: "anthropic", id: "claude-fable-5-1", listed: true, facts: map[string]any{}}
	mustPublish(t, dir, testRun(testNow, docsObs("claude-fable-5-1", fable51), api, docsObs("claude-fable-5-2", fable52)))
	r := testRun(testNow.Add(time.Hour), docsObs("claude-fable-5-2", fable52))
	r.keyed["anthropic"] = false
	if res := mustPublish(t, dir, r); res.snaps["claude-fable-5-1"].Listed {
		t.Error("still listed by an API whose key was taken away")
	}
}
