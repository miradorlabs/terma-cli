package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

func check(harness, version, capability, platform, result string) e2e.CompatRow {
	return e2e.CompatRow{Harness: harness, Version: version, Capability: capability, Platform: platform, Result: result}
}

// A build the history or the catalog already has is no release. A new one counts each
// capability on each platform once, a failure in any test of it failing it, and not what was
// skipped; it is said against the newest build known before it.
func TestAReleaseIsABuildNoNightRanBefore(t *testing.T) {
	hist := map[string]Entry{}
	for _, e := range []Entry{
		{Harness: "codex", Version: "0.161.0", Platform: "linux/amd64", Capability: "relay.telemetry", Result: "pass"},
		{Harness: "codex", Version: "0.162.0", Platform: "linux/amd64", Capability: "relay.telemetry", Result: "pass"},
	} {
		hist[e.key()] = e
	}
	rows := []e2e.CompatRow{
		check("codex", "0.162.0", "relay.telemetry", "linux/amd64", "pass"), // known
		check("codex", "0.162.1", "relay.telemetry", "linux/amd64", "pass"),
		check("codex", "0.162.1", "relay.telemetry", "linux/amd64", "pass"), // a second test of it
		check("codex", "0.162.1", "relay.telemetry", "darwin/arm64", "pass"),
		check("codex", "0.162.1", "relay.desktop", "darwin/arm64", "not run"),
		check("claude", "2.1.297", "relay.telemetry", "darwin/arm64", "pass"),
		check("claude", "2.1.297", "relay.content_withheld", "darwin/arm64", "pass"),
		check("claude", "2.1.297", "relay.content_withheld", "darwin/arm64", "fail"),
	}
	rs := releases(hist, Catalog{}, rows, nil, nil, censusRuns{})
	if len(rs) != 2 || rs[0].Harness != "claude" || rs[1].Version != "0.162.1" {
		t.Fatalf("releases %+v", rs)
	}
	claude, codex := rs[0], rs[1]
	if codex.Previous != "0.162.0" || codex.Checks != 2 || !codex.Passed() || codex.Fields != nil {
		t.Errorf("codex %+v: two checks passed against 0.162.0, not censused", codex)
	}
	if claude.Previous != "" || claude.Checks != 2 || claude.Passed() || len(claude.Failed) != 1 || claude.Failed[0].Capability != "relay.content_withheld" {
		t.Errorf("claude %+v: one of two checks failed, no build before", claude)
	}
	bold := func(s string) string { return "*" + s + "*" }
	ss := statuses(rows, rs, nil)
	if len(ss) != 2 {
		t.Fatalf("statuses %+v", ss)
	}
	if l := ss[1].line(bold); l != "✅ *Codex CLI 0.162.1* · new (was 0.162.0): passes all 2 checks" {
		t.Errorf("codex line %q", l)
	}
	if l := ss[0].line(bold); l != "❌ *Claude Code 2.1.297* · new: fails 1 of 2 checks: Prompts and tool content withheld when the project says so (macOS)" {
		t.Errorf("claude line %q", l)
	}
	skipped := []e2e.CompatRow{check("codex", "0.163.0", "relay.desktop", "darwin/arm64", "not run")}
	if rs := releases(hist, Catalog{}, skipped, nil, nil, censusRuns{}); len(rs) != 0 {
		t.Errorf("a build whose every check was skipped, and no census taken, is a release: %+v", rs)
	}
	census := []e2e.FieldRow{field("0.163.0", "logs/codex.api_request", "model", "safe", time.Now())}
	if rs := releases(hist, Catalog{}, skipped, census, nil, censusRuns{}); len(rs) != 1 || rs[0].Checks != 0 || rs[0].Passed() ||
		statuses(skipped, rs, nil)[0].line(bold) != "⚪ *Codex CLI 0.163.0* · new (was 0.162.0): no checks ran" {
		t.Errorf("a censused build whose every check was skipped: %+v", rs)
	}
}

// A release's line counts what its own census found: what it added, and what it no longer
// sends that the build before it did.
func TestAReleaseSaysWhatItsCensusFound(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.162.0", "logs/codex.api_request", "model", "safe", day),
		field("0.162.0", "logs/codex.api_request", "gone_key", "safe", day),
		field("0.162.0", "metrics/codex.dropped", "value", "safe", day, e2e.KindNumber),
	}, nil)
	tonight := []e2e.FieldRow{
		field("0.162.1", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.162.1", "logs/codex.api_request", "fresh_key", "safe", day.Add(24*time.Hour)),
		field("0.162.1", "logs/codex.new_event", "model", "safe", day.Add(24*time.Hour)),
	}
	_, _, hs := fieldDrift(cat, tonight, censusRuns{}, day.Add(24*time.Hour))
	rows := []e2e.CompatRow{check("codex", "0.162.1", "relay.telemetry", "linux/amd64", "pass")}
	rs := releases(map[string]Entry{}, cat, rows, tonight, hs, censusRuns{})
	if len(rs) != 1 || rs[0].Previous != "0.162.0" {
		t.Fatalf("releases %+v", rs)
	}
	f := rs[0].Fields
	if f == nil || f.Added != 1 || f.NewSurfaces != 1 || f.Removed != 1 || f.Unseen != 1 {
		t.Fatalf("fields %+v: 1 new field, 1 new surface, 1 removed, 1 surface gone", f)
	}
}

// Every digest leads with each harness's verdict: does its newest build send all the
// telemetry terma expects. A build that failed is said once, on its line, not again among the
// compatibility changes; a harness that only sent a new field is green, the field a note.
func TestTheDigestLeadsWithEachHarnessesVerdict(t *testing.T) {
	d := Drift{GeneratedAt: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)}
	if !strings.Contains(d.markdown(), "No harness was tested tonight") || !strings.Contains(slackDigestText(t, d), "No harness was tested tonight") {
		t.Errorf("a night that tested nothing does not say so:\n%s", d.markdown())
	}
	d.Status = []Status{
		{Harness: "claude", Name: "Claude Code", Version: "2.1.297", Checks: 3, Failed: []FailedCheck{{Capability: "relay.telemetry", Platform: "linux/amd64"}},
			Release: &Release{Harness: "claude", Name: "Claude Code", Version: "2.1.297", Previous: "2.1.296"}},
		{Harness: "codex", Name: "Codex CLI", Version: "0.162.1", Checks: 2, OlderFailed: []string{"0.162.0"}},
	}
	d.Compat = []CompatChange{
		{Harness: "claude", Version: "2.1.297", Platform: "linux/amd64", Capability: "relay.telemetry", To: "fail"},
		{Harness: "codex", Version: "0.162.0", Platform: "linux/amd64", Capability: "relay.telemetry", From: "pass", To: "fail"},
	}
	if d.Quiet() {
		t.Error("a night a harness failed is quiet")
	}
	md := d.markdown()
	if strings.Count(md, "Claude Code 2.1.297") != 1 || !strings.Contains(md, "Codex CLI 0.162.0") || !strings.Contains(md, "older builds failing: 0.162.0") {
		t.Errorf("a failure is said twice, or an older build's not at all:\n%s", md)
	}
	s := slackDigestText(t, d)
	if !strings.HasPrefix(s, "Harness drift — 2026-10-12\n*Does each harness send all the telemetry terma expects?*\n❌ *Claude Code 2.1.297* · new (was 2.1.296): fails 1 of 3 checks") ||
		strings.Index(s, "Does each harness") > strings.Index(s, "Compatibility changes") {
		t.Errorf("the verdicts do not lead:\n%s", s)
	}
	if got := d.slack()["text"]; got != "Harness drift: 1 harness not sending what terma expects" {
		t.Errorf("summary %q", got)
	}
	// Only a new field: every harness green, the field noted, and no harness said failing.
	d.Status[0].Failed, d.Status[1].OlderFailed, d.Compat = nil, nil, nil
	d.Harnesses = []HarnessDrift{{Harness: "claude", Name: "Claude Code", Version: "2.1.297", Previous: "2.1.296",
		Added:    []FieldChange{{Surface: "logs/tool_decision", Key: "permission_mode", Class: "unclassified"}},
		Withheld: []FieldChange{{Surface: "logs/tool_decision", Key: "permission_mode"}}}}
	s = slackDigestText(t, d)
	if !strings.Contains(s, "✅ *Claude Code 2.1.297* · new (was 2.1.296): passes all 3 checks\n✅ *Codex CLI 0.162.1*: passes all 2 checks") ||
		!strings.Contains(s, "*To note*\nNew fields, withheld until classified: Claude Code: `permission_mode`.") || strings.Contains(s, "❌") {
		t.Errorf("a new field is not a note beside green verdicts:\n%s", s)
	}
	if got := d.slack()["text"]; got != "Harness drift: every harness sends what terma expects" {
		t.Errorf("summary %q", got)
	}
	if d.Status[1].Checks = 0; d.Quiet() {
		t.Error("a night a harness ran no checks is quiet")
	}
}

// slackItem is one block of a Slack payload, or one attachment, with its bar's colour.
type slackItem struct{ colour, text string }

// slackItems are d's Slack payload's blocks, then its attachments, as Slack shows them.
func slackItems(t *testing.T, d Drift) []slackItem {
	t.Helper()
	data, err := json.Marshal(d.slack())
	if err != nil {
		t.Fatal(err)
	}
	type block struct {
		Text     struct{ Text string }   `json:"text"`
		Elements []struct{ Text string } `json:"elements"`
	}
	var p struct {
		Blocks      []block `json:"blocks"`
		Attachments []struct {
			Color  string  `json:"color"`
			Blocks []block `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	textOf := func(b block) string {
		s := b.Text.Text
		for _, e := range b.Elements {
			s += e.Text
		}
		return s
	}
	var out []slackItem
	for _, b := range p.Blocks {
		out = append(out, slackItem{"", textOf(b)})
	}
	for _, a := range p.Attachments {
		var texts []string
		for _, b := range a.Blocks {
			texts = append(texts, textOf(b))
		}
		out = append(out, slackItem{a.Color, strings.Join(texts, "\n")})
	}
	return out
}

// slackDigestText is the text of d's Slack payload, in the order Slack shows it.
func slackDigestText(t *testing.T, d Drift) string {
	t.Helper()
	var b strings.Builder
	for _, it := range slackItems(t, d) {
		b.WriteString(it.text + "\n")
	}
	return b.String()
}

// A harness's changes are its headline on Slack's own bar, for a colour says nothing of how
// the build did, with what neither came nor went (the newly withheld); then one green
// attachment of all that came and one red of all that went.
func TestSlackColoursWhatCameAndWent(t *testing.T) {
	d := Drift{GeneratedAt: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC), Harnesses: []HarnessDrift{{
		Harness: "codex", Name: "Codex CLI", Version: "0.162.1", Previous: "0.162.0",
		NewSurfaces: []SurfaceChange{{Surface: "logs/codex.new", Keys: 1, NewKeys: 1}},
		Added:       []FieldChange{{Surface: "logs/codex.api_request", Key: "region", Class: "safe"}},
		Removed:     []FieldChange{{Surface: "logs/codex.api_request", Key: "gone", Class: "safe"}},
		Unseen:      []GoneSurface{{Surface: "metrics/codex.dropped"}},
		Withheld:    []FieldChange{{Surface: "logs/codex.api_request", Key: "state"}},
	}}}
	items := slackItems(t, d)
	i := slices.IndexFunc(items, func(it slackItem) bool { return strings.HasPrefix(it.text, "*Codex CLI 0.162.1") })
	if i < 0 || len(items) != i+3 {
		t.Fatalf("items %+v: want the headline, what came and what went, last", items)
	}
	head, came, went := items[i], items[i+1], items[i+2]
	if head.colour != "" || !strings.Contains(head.text, "_Newly withheld by the relay, unclassified_\n• `state`") {
		t.Errorf("the headline, with the withheld, on Slack's own bar: %+v", head)
	}
	if came.colour != addedColour || strings.Contains(came.text, "*Codex CLI") ||
		!strings.Contains(came.text, "_New fields_\n`+region`") || strings.Contains(came.text, addedMark) {
		t.Errorf("what came: %+v", came)
	}
	if went.colour != removedColour || !strings.Contains(went.text, "_Surfaces no longer sent_\n`-metrics/codex.dropped`") || !strings.Contains(went.text, "`-gone`") {
		t.Errorf("what went: %+v", went)
	}
	// The summary first, then under a divider the harnesses' changes, with nothing after them.
	d.Harnesses = append(d.Harnesses, HarnessDrift{Harness: "pi", Name: "Pi", Version: "0.84.2", Previous: "0.84.2"})
	d.Link = "https://ci/run/4"
	items = slackItems(t, d)
	at := func(prefix string) int {
		return slices.IndexFunc(items, func(it slackItem) bool { return strings.HasPrefix(it.text, prefix) })
	}
	d.Status = []Status{{Harness: "pi", Name: "Pi", Version: "0.84.2", Checks: 1}}
	items = slackItems(t, d)
	verdicts, run, heading, codex := at("*Does each harness send all the telemetry terma expects?*\n✅ *Pi 0.84.2*: passes its 1 check"), at("<https://ci/run/4|The run>"), at("*Field changes by harness*"), at("*Codex CLI 0.162.1")
	if verdicts < 0 || run < 0 || !(verdicts < run && run < heading-1 && heading < codex) || items[heading-1].text != "" || items[len(items)-1].colour != removedColour {
		t.Errorf("not summary, divider, then the harnesses: %+v", items)
	}
	if md := d.markdown(); !strings.Contains(md, "- 🟢 `+region`") || !strings.Contains(md, "- 🔴 `-gone`") || !strings.Contains(md, "- `state`") {
		t.Errorf("drift.md does not mark what came and went:\n%s", md)
	}
}

// A build whose first night skipped every check is no release yet: its first night that runs
// is, though the history has its skipped checks.
func TestASkippedFirstNightIsNoRelease(t *testing.T) {
	hist := map[string]Entry{}
	for _, e := range []Entry{
		{Harness: "codex", Version: "0.162.0", Platform: "linux/amd64", Capability: "relay.telemetry", Result: "pass"},
		{Harness: "codex", Version: "0.163.0", Platform: "linux/amd64", Capability: "relay.telemetry", Result: "not run"},
	} {
		hist[e.key()] = e
	}
	rs := releases(hist, Catalog{}, []e2e.CompatRow{check("codex", "0.163.0", "relay.telemetry", "linux/amd64", "fail")}, nil, nil, censusRuns{})
	if len(rs) != 1 || rs[0].Version != "0.163.0" || rs[0].Previous != "0.162.0" || rs[0].Passed() {
		t.Errorf("releases %+v: 0.163.0's first night that ran, failed", rs)
	}
}

// Two releases of one harness in a night: a key both send is the older's news, and each says
// whether its own census was partial; a harness new to the catalog is a first census in each.
func TestTwoReleasesInANight(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", day)}, nil)
	tonight := []e2e.FieldRow{
		field("0.162.1", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.162.1", "logs/codex.api_request", "k1", "safe", day.Add(24*time.Hour)),
		field("0.162.2", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour)),
		field("0.162.2", "logs/codex.api_request", "k1", "safe", day.Add(24*time.Hour)),
		field("0.162.2", "logs/codex.api_request", "k2", "safe", day.Add(24*time.Hour)),
	}
	ran := censusRan([]e2e.CensusRun{{Harness: "codex", Version: "0.162.1", Failed: true}, {Harness: "codex", Version: "0.162.2"}})
	_, _, hs := fieldDrift(cat, tonight, ran, day.Add(24*time.Hour))
	rs := releases(map[string]Entry{}, cat, nil, tonight, hs, ran)
	if len(rs) != 2 {
		t.Fatalf("releases %+v", rs)
	}
	older, newer := rs[0].Fields, rs[1].Fields
	if older.Added != 1 || !older.Partial || newer.Added != 1 || newer.Partial {
		t.Errorf("0.162.1 %+v, 0.162.2 %+v: k1 the older's, k2 the newer's; the older's census partial", older, newer)
	}
	_, _, hs = fieldDrift(Catalog{}, tonight, ran, day.Add(24*time.Hour))
	for _, r := range releases(map[string]Entry{}, Catalog{}, nil, tonight, hs, ran) {
		if !r.Fields.First || r.Fields.Keys == 0 {
			t.Errorf("%s %+v: a harness new to the catalog is a first census", r.Version, r.Fields)
		}
	}
}

// Failures on two platforms with one short name say which each is.
func TestTwoLinuxFailuresSayTheirArchitecture(t *testing.T) {
	failed := []FailedCheck{{Capability: "relay.telemetry", Platform: "linux/amd64"}, {Capability: "relay.telemetry", Platform: "linux/arm64"}}
	if got := strings.Join(failedNames(failed), "; "); !strings.Contains(got, "(linux/amd64); Telemetry reaches the project through the local relay (linux/arm64)") {
		t.Errorf("names %q", got)
	}
	if got := strings.Join(failedNames(failed[:1]), "; "); !strings.Contains(got, "(Linux)") {
		t.Errorf("one Linux failure: %q", got)
	}
}

// A release links the comparison of its source with the build known before it, where the
// source is public, though the night took no census of it; a harness's own does not.
func TestAReleaseLinksItsSourceWithoutACensus(t *testing.T) {
	d := Drift{Releases: []Release{
		{Harness: "codex", Name: "Codex CLI", Version: "0.163.0", Previous: "0.162.1", Checks: 1},
		{Harness: "codex", Name: "Codex CLI", Version: "0.1.0", Checks: 1}, // no build before it
		{Harness: "claude", Name: "Claude Code", Version: "2.1.297", Previous: "2.1.296", Checks: 1},
	}}
	linkSources(&d, nil)
	if got := d.Releases[0].Compare; got != "https://github.com/openai/codex/compare/rust-v0.162.1...rust-v0.163.0" {
		t.Errorf("codex compare %q", got)
	}
	if d.Releases[1].Compare != "" || d.Releases[2].Compare != "" {
		t.Errorf("a first build, or a harness with no public source, linked a comparison: %+v", d.Releases)
	}
	st := Status{Name: "Codex CLI", Version: "0.163.0", Checks: 1, Release: &d.Releases[0]}
	if l := st.line(func(s string) string { return s }); !strings.Contains(renderLinks(l, false), "· new (was 0.162.1, [diff](https://github.com/openai/codex/compare/rust-v0.162.1...rust-v0.163.0))") {
		t.Errorf("line %q", renderLinks(l, false))
	}
}

// Lists that share a bar are a section each: new surfaces and new fields, each within a
// section's limit, would together pass it, and be cut.
func TestListsOnOneBarAreSectionsEach(t *testing.T) {
	h := HarnessDrift{Harness: "codex", Name: "Codex CLI", Version: "0.163.0", Previous: "0.162.0"}
	long := "logs/" + strings.Repeat("s", 150)
	for i := range maxListed {
		h.NewSurfaces = append(h.NewSurfaces, SurfaceChange{Surface: fmt.Sprint(long, i), Keys: 1, NewKeys: 1})
		h.Added = append(h.Added, FieldChange{Surface: long, Key: fmt.Sprint("key_", i), Class: "safe"})
	}
	data, err := json.Marshal(Drift{GeneratedAt: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC), Harnesses: []HarnessDrift{h}}.slack())
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Attachments []struct {
			Color  string `json:"color"`
			Blocks []struct {
				Text struct{ Text string } `json:"text"`
			} `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	var green []string
	for _, a := range p.Attachments {
		if a.Color == addedColour {
			for _, b := range a.Blocks {
				green = append(green, b.Text.Text)
			}
		}
	}
	if len(green) != 2 {
		t.Fatalf("the green bar's sections %d: want new surfaces and new fields apart", len(green))
	}
	for _, s := range green {
		if len(s) > sectionLimit || strings.HasSuffix(s, "\n…") || !strings.Contains(s, "11") {
			t.Errorf("a section cut short (%d bytes): …%s", len(s), s[max(0, len(s)-80):])
		}
	}
}

// A build older than the newest known is no release, though nothing knows it: the catalog
// forgets old censuses, and a build it forgot, run again, is no news.
func TestAnOlderBuildIsNoRelease(t *testing.T) {
	cat := Catalog{Censuses: []Census{{Harness: "codex", Version: "0.170.0"}}}
	rows := []e2e.CompatRow{check("codex", "0.165.0", "relay.telemetry", "linux/amd64", "pass"), check("codex", "0.171.0", "relay.telemetry", "linux/amd64", "pass")}
	rs := releases(map[string]Entry{}, cat, rows, nil, nil, censusRuns{})
	if len(rs) != 1 || rs[0].Version != "0.171.0" || rs[0].Previous != "0.170.0" {
		t.Errorf("releases %+v: only 0.171.0, against 0.170.0", rs)
	}
}
