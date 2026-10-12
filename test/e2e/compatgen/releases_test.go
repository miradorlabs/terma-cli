package main

import (
	"encoding/json"
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
	rs := releases(hist, Catalog{}, rows, nil, nil)
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
	if l := codex.line(bold); !strings.HasPrefix(l, "✅ *Codex CLI 0.162.1 (was 0.162.0)*: passes all 2 checks · fields: not censused") {
		t.Errorf("codex line %q", l)
	}
	if l := claude.line(bold); !strings.Contains(l, "❌ *Claude Code 2.1.297 (first build seen)*: fails 1 of 2 checks: Prompts and tool content withheld when the project says so (macOS)") {
		t.Errorf("claude line %q", l)
	}
	if rs := releases(hist, Catalog{}, []e2e.CompatRow{check("codex", "0.163.0", "relay.desktop", "darwin/arm64", "not run")}, nil, nil); rs[0].Checks != 0 || rs[0].Passed() ||
		!strings.Contains(rs[0].line(bold), "⚪ *Codex CLI 0.163.0 (was 0.162.0)*: no checks ran") {
		t.Errorf("a release whose every check was skipped: %+v", rs[0])
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
	rs := releases(map[string]Entry{}, cat, rows, tonight, hs)
	if len(rs) != 1 || rs[0].Previous != "0.162.0" {
		t.Fatalf("releases %+v", rs)
	}
	f := rs[0].Fields
	if f == nil || f.Added != 1 || f.NewSurfaces != 1 || f.Removed != 1 || f.Unseen != 1 {
		t.Fatalf("fields %+v: 1 new field, 1 new surface, 1 removed, 1 surface gone", f)
	}
	if got := f.said(); got != "🟢 +1 surface, +1 field, 🔴 -1 field, -1 surface" {
		t.Errorf("said %q", got)
	}
}

// Every digest says whether there were releases; a release that failed is said once, on its
// line, not again among the compatibility changes, and makes no quiet night.
func TestTheDigestLeadsWithTheReleases(t *testing.T) {
	d := Drift{GeneratedAt: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)}
	if !strings.Contains(d.markdown(), "None tonight") || !strings.Contains(slackText(t, d), "*New releases:* none tonight") {
		t.Errorf("no releases is not said:\n%s", d.markdown())
	}
	d.Harnesses = []HarnessDrift{{Harness: "gemini", Name: "Gemini CLI", Version: "0.63.0", Previous: "0.62.0"}}
	if u := d.unchanged(); u != "Gemini CLI 0.63.0 (new build, was 0.62.0)" {
		t.Errorf("unchanged %q: a new build no release line says is said new here", u)
	}
	d.Releases = []Release{{Harness: "gemini", Name: "Gemini CLI", Version: "0.63.0", Previous: "0.62.0", Checks: 1}}
	if u := d.unchanged(); u != "Gemini CLI 0.63.0" {
		t.Errorf("unchanged %q: a release is said again", u)
	}
	d.Harnesses = nil
	d.Releases = []Release{{Harness: "claude", Name: "Claude Code", Version: "2.1.297", Previous: "2.1.296", Checks: 3,
		Failed: []FailedCheck{{Capability: "relay.telemetry", Platform: "linux/amd64"}}}}
	d.Compat = []CompatChange{
		{Harness: "claude", Version: "2.1.297", Platform: "linux/amd64", Capability: "relay.telemetry", To: "fail"},
		{Harness: "codex", Version: "0.162.0", Platform: "linux/amd64", Capability: "relay.telemetry", From: "pass", To: "fail"},
	}
	if d.Quiet() {
		t.Error("a night whose release failed is quiet")
	}
	md := d.markdown()
	if strings.Count(md, "Claude Code 2.1.297") != 1 || !strings.Contains(md, "Codex CLI 0.162.0") {
		t.Errorf("a release's failure is said twice, or another change not at all:\n%s", md)
	}
	if s := slackText(t, d); strings.Index(s, "New releases") > strings.Index(s, "Compatibility changes") {
		t.Errorf("the releases do not lead:\n%s", s)
	}
	if d.Compat = nil; d.Quiet() {
		t.Error("a night whose release failed, its history saying nothing of it, is quiet")
	}
	if d.Releases[0].Checks = 0; d.Quiet() {
		t.Error("a night whose release ran no checks is quiet")
	}
	d.Releases[0].Checks, d.Releases[0].Failed = 3, nil
	if !d.Quiet() {
		t.Error("a release that passed every check makes the night not quiet")
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

// slackText is the text of d's Slack payload, in the order Slack shows it.
func slackText(t *testing.T, d Drift) string {
	t.Helper()
	var b strings.Builder
	for _, it := range slackItems(t, d) {
		b.WriteString(it.text + "\n")
	}
	return b.String()
}

// A harness's changes are its headline on Slack's own bar, for a colour says nothing of how
// the build did; then one green attachment of all that came and one red of all that went; what
// is neither, the newly withheld, keeps Slack's own bar.
func TestSlackColoursWhatCameAndWent(t *testing.T) {
	d := Drift{GeneratedAt: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC), Harnesses: []HarnessDrift{{
		Harness: "codex", Name: "Codex CLI", Version: "0.162.1", Previous: "0.162.0",
		NewSurfaces: []SurfaceChange{{Surface: "logs/codex.new", Keys: 1, NewKeys: 1}},
		Added:       []FieldChange{{Surface: "logs/codex.api_request", Key: "region", Class: "safe"}},
		Removed:     []FieldChange{{Surface: "logs/codex.api_request", Key: "gone", Class: "safe"}},
		Unseen:      []GoneSurface{{Surface: "metrics/codex.dropped"}},
		Withheld:    []FieldChange{{Surface: "logs/codex.api_request", Key: "state"}},
	}}}
	var coloured []slackItem
	for _, it := range slackItems(t, d) {
		if strings.Contains(it.text, "_New surfaces_") || strings.Contains(it.text, "_Removed fields_") || strings.Contains(it.text, "_Newly withheld") {
			coloured = append(coloured, it)
		}
	}
	if len(coloured) != 3 {
		t.Fatalf("attachments %+v: want what came, what went, and the withheld", coloured)
	}
	came, went, withheld := coloured[0], coloured[1], coloured[2]
	items := slackItems(t, d)
	if i := slices.Index(items, came); i < 1 || items[i-1].colour != "" || !strings.HasPrefix(items[i-1].text, "*Codex CLI 0.162.1") {
		t.Errorf("the headline is not on its own, uncoloured, before what came: %+v", items)
	}
	if came.colour != addedColour || strings.Contains(came.text, "*Codex CLI") ||
		!strings.Contains(came.text, "_New fields_\n`+region`") || strings.Contains(came.text, addedMark) {
		t.Errorf("what came: %+v", came)
	}
	if went.colour != removedColour || strings.Contains(went.text, "*Codex CLI") ||
		!strings.Contains(went.text, "_Surfaces no longer sent_\n`-metrics/codex.dropped`") || !strings.Contains(went.text, "`-gone`") {
		t.Errorf("what went: %+v", went)
	}
	if withheld.colour != "" || !strings.Contains(withheld.text, "• `state`") {
		t.Errorf("the withheld: %+v", withheld)
	}
	// The summary first, then under a divider the harnesses' changes, with nothing after them.
	d.Harnesses = append(d.Harnesses, HarnessDrift{Harness: "pi", Name: "Pi", Version: "0.84.2", Previous: "0.84.2"})
	d.Link = "https://ci/run/4"
	items = slackItems(t, d)
	at := func(prefix string) int {
		return slices.IndexFunc(items, func(it slackItem) bool { return strings.HasPrefix(it.text, prefix) })
	}
	unchanged, run, heading, head := at("_Unchanged:_ Pi 0.84.2"), at("<https://ci/run/4|The run>"), at("*Field changes by harness*"), at("*Codex CLI 0.162.1")
	if unchanged < 0 || run < 0 || !(unchanged < run && run < heading-1 && heading < head) || items[heading-1].text != "" || !strings.HasPrefix(items[len(items)-1].text, "_Newly withheld") {
		t.Errorf("not summary, divider, then the harnesses: %+v", items)
	}
	if md := d.markdown(); !strings.Contains(md, "- 🟢 `+region`") || !strings.Contains(md, "- 🔴 `-gone`") || !strings.Contains(md, "- `state`") {
		t.Errorf("drift.md does not mark what came and went:\n%s", md)
	}
}
