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

// A harness's verdict is its newest build that ran checks: a newer one whose every check was
// skipped is named beside it, never mistaken for a failure, nor allowed to hide the older's.
// Red comes first, and a change from before says so.
func TestStatusEdges(t *testing.T) {
	bold := func(s string) string { return s }
	rows := []e2e.CompatRow{
		check("claude", "2.1.300", "relay.telemetry", "darwin/arm64", "pass"),
		check("claude", "2.1.301", "relay.telemetry", "darwin/arm64", "not run"),
		check("codex", "0.163.0", "relay.telemetry", "linux/amd64", "fail"),
		check("codex", "0.163.0", "relay.content_withheld", "darwin/arm64", "pass"),
		check("pi", "1.3.0", "relay.telemetry", "darwin/arm64", "pass"),
		check("t3", "0.1.0", "relay.telemetry", "darwin/arm64", "not run"),
	}
	compat := []CompatChange{
		{Harness: "codex", Version: "0.163.0", Platform: "linux/amd64", Capability: "relay.telemetry", From: "pass", To: "fail"},
		{Harness: "pi", Version: "1.3.0", Platform: "darwin/arm64", Capability: "relay.telemetry", From: "fail", To: "pass"},
	}
	hist := map[string]Entry{}
	for _, e := range []Entry{
		{Harness: "pi", Version: "1.3.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "fail"},
	} {
		hist[e.key()] = e
	}
	ss := statuses(rows, []Release{{Harness: "claude", Version: "2.1.301"}}, compat, hist)
	var lines []string
	for _, s := range ss {
		lines = append(lines, s.line(bold))
	}
	want := []string{
		"❌ Codex CLI 0.163.0: fails 1 of 2 checks: Telemetry reaches the project through the local relay (Linux) · newly failing, passed before",
		"⚪ T3 Code 0.1.0: no checks ran",
		"✅ Claude Code 2.1.300: passes its 1 check · 2.1.301 (new) ran no checks",
		"✅ Pi 1.3.0: passes its 1 check · fixed, failed before",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if got := (Drift{Status: ss}).summary(); got != "Harness drift: 1 harness not sending what terma expects, 1 harness ran no checks" {
		t.Errorf("summary %q", got)
	}
	if got := (Drift{Status: ss[2:]}).summary(); got != "Harness drift: every harness sends what terma expects" {
		t.Errorf("summary %q", got)
	}
	if got := (Drift{Status: ss[1:]}).summary(); got != "Harness drift: 1 harness ran no checks; the rest send what terma expects" {
		t.Errorf("summary %q", got)
	}
	if got := (Drift{Status: ss[1:2]}).summary(); got != "Harness drift: no harness ran its checks tonight" {
		t.Errorf("summary %q: no harness is left to send anything", got)
	}
}

// However many harnesses fail (sixteen, four checks each, pass one section), every verdict reaches Slack: the list is split across sections,
// never clipped.
func TestEveryVerdictReachesSlack(t *testing.T) {
	d := Drift{GeneratedAt: time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)}
	for i := range 16 {
		d.Status = append(d.Status, Status{Harness: fmt.Sprint("h", i), Name: fmt.Sprintf("Harness %02d", i), Version: "1.0.0", Checks: 20,
			Failed: []FailedCheck{{Capability: "relay.telemetry", Platform: "darwin/arm64"}, {Capability: "relay.content_withheld", Platform: "darwin/arm64"},
				{Capability: "relay.content_allowed", Platform: "linux/amd64"}, {Capability: "relay.cold_start", Platform: "linux/amd64"}}})
	}
	items := slackItems(t, d)
	text := slackDigestText(t, d)
	for i := range 16 {
		if !strings.Contains(text, fmt.Sprintf("❌ *Harness %02d 1.0.0*", i)) {
			t.Errorf("Harness %02d's verdict is missing", i)
		}
	}
	for _, it := range items {
		if len(it.text) > sectionLimit || strings.HasSuffix(it.text, "\n…") {
			t.Errorf("a section cut short (%d bytes)", len(it.text))
		}
	}
}

// A build is fixed only where every check it failed before passed tonight: one skipped tonight
// may still fail.
func TestFixedNeedsEveryEarlierFailureToPass(t *testing.T) {
	hist := map[string]Entry{}
	for _, e := range []Entry{
		{Harness: "pi", Version: "1.3.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "fail"},
		{Harness: "pi", Version: "1.3.0", Platform: "darwin/arm64", Capability: "relay.content_withheld", Result: "fail"},
	} {
		hist[e.key()] = e
	}
	rows := []e2e.CompatRow{
		check("pi", "1.3.0", "relay.telemetry", "darwin/arm64", "pass"),
		check("pi", "1.3.0", "relay.content_withheld", "darwin/arm64", "not run"),
	}
	if s := statuses(rows, nil, nil, hist)[0]; s.Fixed {
		t.Errorf("%+v: fixed, with a failure skipped tonight", s)
	}
	rows[1].Result = "pass"
	if s := statuses(rows, nil, nil, hist)[0]; !s.Fixed {
		t.Errorf("%+v: not fixed, every earlier failure passing", s)
	}
}

// A key a tripwire met that the census of the harness that met it does not say withheld, the
// Linux machines' say, is to note; one that census found, new or before, is said with it.
func TestUnclassifiedKeysTheChecksMetAreToNote(t *testing.T) {
	hs := []HarnessDrift{
		{Harness: "codex", Name: "Codex",
			Withheld:      []FieldChange{{Surface: "metric:codex.tools.registered", Key: "tool_mode"}},
			StillWithheld: []FieldChange{{Surface: "log:codex.decision", Key: "decision_type"}, {Surface: "resource", Key: "host.id"}}},
		{Harness: "claude", Name: "Claude Code", StillWithheld: []FieldChange{{Surface: "log:claude_code.tool_decision", Key: "state"}}},
	}
	met := map[MetKey]bool{
		{"codex", "tool_mode"}: true, {"codex", "decision_type"}: true, {"codex", "resource/host.id"}: true,
		{"codex", "exposure"}: true,
		// Claude's census has state withheld, Codex's does not: Codex's is news.
		{"codex", "state"}: true,
		// A scenario that proves no harness, or one with no census of its own (T3 Code runs
		// Codex): any census that has it withheld says it.
		{"", "state"}: true, {"", "overflowed"}: true, {"t3", "decision_type"}: true,
	}
	d := Drift{Harnesses: hs, Unclassified: unclassified(met, hs)}
	want := []MetKey{{"codex", "exposure"}, {"codex", "state"}, {"", "overflowed"}}
	if !slices.Equal(d.Unclassified, want) {
		t.Fatalf("unclassified %+v, want %+v", d.Unclassified, want)
	}
	// A night that met only keys their harness's census says withheld is quiet.
	if q := (Drift{Harnesses: []HarnessDrift{{Harness: "pi"}}, Unclassified: unclassified(map[MetKey]bool{{"codex", "decision_type"}: true}, hs)}); !q.Quiet() {
		t.Errorf("%+v: a night meeting only keys withheld before is not quiet", q.Unclassified)
	}
	if d.Quiet() {
		t.Error("a night that met an unclassified key is quiet")
	}
	said := "Unclassified keys the checks met, withheld until classified: Codex CLI: `exposure`, `state`; other scenarios: `overflowed`."
	data, _ := json.Marshal(d.slack())
	if !strings.Contains(string(data), said) {
		t.Errorf("slack does not say %q:\n%s", said, data)
	}
	if md := d.markdown(); !strings.Contains(md, said) {
		t.Errorf("drift.md does not say %q:\n%s", said, md)
	}
}
