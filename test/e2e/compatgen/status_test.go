package main

import (
	"fmt"
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
	ss := statuses(rows, nil, compat)
	var lines []string
	for _, s := range ss {
		lines = append(lines, s.line(bold))
	}
	want := []string{
		"❌ Codex CLI 0.163.0: fails 1 of 2 checks: Telemetry reaches the project through the local relay (Linux) · newly failing, passed before",
		"⚪ T3 Code 0.1.0: no checks ran",
		"✅ Claude Code 2.1.300: passes all 1 check · 2.1.301 ran no checks",
		"✅ Pi 1.3.0: passes all 1 check · fixed, failed before",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if got := (Drift{Status: ss}).summary(); got != "Harness drift: 1 harness not sending what terma expects, 1 ran no checks" {
		t.Errorf("summary %q", got)
	}
	if got := (Drift{Status: ss[2:]}).summary(); got != "Harness drift: every harness sends what terma expects" {
		t.Errorf("summary %q", got)
	}
	if got := (Drift{Status: ss[1:2]}).summary(); got != "Harness drift: 1 ran no checks" {
		t.Errorf("summary %q", got)
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
