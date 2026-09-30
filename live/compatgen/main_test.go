package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/live"
)

func writeRun(t *testing.T, dir, name string, rows []live.CompatRow) string {
	t.Helper()
	p := filepath.Join(dir, name)
	data, _ := json.Marshal(rows)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Runs accumulate: a newer result replaces an older one, the first pass is kept, a run
// that did not get to a capability does not erase what was known, and the matrix
// shows the newest builds first with each platform named.
func TestCompatHistory(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	hist := filepath.Join(dir, "history.json")
	md := filepath.Join(dir, "COMPATIBILITY.md")
	web := filepath.Join(dir, "compat.json")
	r1 := writeRun(t, dir, "r1.json", []live.CompatRow{
		{Harness: "codex", Version: "0.157.0", Capability: "relay.telemetry", Result: "pass", Platform: "darwin/arm64", At: day1},
		{Harness: "codex", Version: "0.158.0", Capability: "relay.telemetry", Result: "fail", Platform: "linux/arm64", At: day1},
		{Harness: "codex", Version: "0.158.0", Capability: "relay.daemon", Result: "pass", Platform: "linux/arm64", At: day1},
	})
	if err := run([]string{r1}, hist, filepath.Join(dir, "none.json"), md, web, day1); err != nil {
		t.Fatal(err)
	}
	r2 := writeRun(t, dir, "r2.json", []live.CompatRow{
		{Harness: "codex", Version: "0.158.0", Capability: "relay.telemetry", Result: "pass", Platform: "linux/arm64", At: day2},
		{Harness: "codex", Version: "0.158.0", Capability: "relay.daemon", Result: "not run", Platform: "linux/arm64", At: day2},
		{Harness: "codex", Version: "0.158.0", Capability: "relay.telemetry", Result: "pass", Platform: "darwin/arm64", At: day2},
	})
	if err := run([]string{r2}, hist, filepath.Join(dir, "none.json"), md, web, day2); err != nil {
		t.Fatal(err)
	}
	var entries []Entry
	data, _ := os.ReadFile(hist)
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range entries {
		got[e.key()] = e
	}
	if e := got["codex|0.158.0|linux/arm64|relay.telemetry"]; e.Result != "pass" || !e.FirstPassed.Equal(day2) {
		t.Errorf("a later pass did not replace the failure: %+v", e)
	}
	if e := got["codex|0.158.0|linux/arm64|relay.daemon"]; e.Result != "pass" || !e.LastRun.Equal(day1) {
		t.Errorf("a run that skipped the capability erased it: %+v", e)
	}
	if e := got["codex|0.157.0|darwin/arm64|relay.telemetry"]; e.Result != "pass" {
		t.Errorf("an older build no longer tested was lost: %+v", e)
	}
	text, _ := os.ReadFile(md)
	s := string(text)
	if i, j := strings.Index(s, "0.158.0 |"), strings.Index(s, "0.157.0 |"); i < 0 || j < 0 || i > j {
		t.Errorf("builds are not newest first:\n%s", s)
	}
	if !strings.Contains(s, "✅ Linux, macOS · 2026-09-30") {
		t.Errorf("a cell does not name its platforms and date:\n%s", s)
	}
	if !strings.Contains(s, "| Codex CLI | 0.158.0 | 2 of 2 |") {
		t.Errorf("the summary is wrong:\n%s", s)
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"0.9.0", "0.10.0"}, {"2.1.99", "2.1.285"}, {"0.2.0-rc.2", "0.2.0"}, {"0.2.0-rc.1", "0.2.0-rc.2"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("versionLess(%s, %s)", c[0], c[1])
		}
	}
}
