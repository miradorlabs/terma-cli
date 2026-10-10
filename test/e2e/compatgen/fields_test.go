package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

func writeFieldRun(t *testing.T, dir, name string, rows []e2e.FieldRow) string {
	t.Helper()
	p := filepath.Join(dir, name)
	data, _ := json.Marshal(rows)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func field(version, surface, key, class string, at time.Time, kinds ...string) e2e.FieldRow {
	if len(kinds) == 0 {
		kinds = []string{e2e.KindText}
	}
	return e2e.FieldRow{Harness: "codex", Version: version, Surface: surface, Key: key, Class: class, Kinds: kinds, Platform: "darwin/arm64", At: at}
}

// A run's census against the catalog: a key no build had is new, a key the previous newest
// build had on a surface still seen is removed, a surface not seen is said apart, and an
// unclassified key with text values is withheld while one with only numbers passes. The
// catalog then has every build each key was seen in.
func TestFieldDriftAndCatalog(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	c := config{history: filepath.Join(dir, "history.json"), md: filepath.Join(dir, "C.md"), json: filepath.Join(dir, "c.json"),
		catalog: filepath.Join(dir, "fields.json"), fieldsMD: filepath.Join(dir, "FIELDS.md"), digest: filepath.Join(dir, "report"), link: "https://ci/run/1"}
	c.fieldRuns = []string{writeFieldRun(t, dir, "f1.json", []e2e.FieldRow{
		field("0.161.0", "logs/codex.api_request", "model", "safe", day1),
		field("0.161.0", "logs/codex.api_request", "old_key", "safe", day1),
		field("0.161.0", "metrics/codex.turn.e2e_duration_ms", "originator", "safe", day1),
	})}
	if err := run(c, day1); err != nil {
		t.Fatal(err)
	}
	c.fieldRuns = []string{writeFieldRun(t, dir, "f2.json", []e2e.FieldRow{
		field("0.162.0", "logs/codex.api_request", "model", "safe", day2),
		field("0.162.0", "logs/codex.api_request", "product_sku", "unclassified", day2),
		field("0.162.0", "logs/codex.api_request", "turn_count", "unclassified", day2, e2e.KindNumber),
	})}
	if err := run(c, day2); err != nil {
		t.Fatal(err)
	}
	var d Drift
	data, err := os.ReadFile(filepath.Join(dir, "report", "drift.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Harnesses) != 1 {
		t.Fatalf("drift = %+v", d)
	}
	h := d.Harnesses[0]
	keys := func(cs []FieldChange) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Key)
		}
		return out
	}
	if h.Version != "0.162.0" || h.Previous != "0.161.0" {
		t.Errorf("builds %s, previous %s", h.Version, h.Previous)
	}
	if got := keys(h.Added); !slices.Equal(got, []string{"product_sku", "turn_count"}) {
		t.Errorf("added %v", got)
	}
	if got := keys(h.Removed); !slices.Equal(got, []string{"old_key"}) {
		t.Errorf("removed %v", got)
	}
	if got := keys(h.Withheld); !slices.Equal(got, []string{"product_sku"}) {
		t.Errorf("withheld %v: an unclassified number passes", got)
	}
	// The next night, the same key withheld is a reminder, not a change.
	c.fieldRuns = []string{writeFieldRun(t, dir, "f3.json", []e2e.FieldRow{
		field("0.162.0", "logs/codex.api_request", "model", "safe", day2.Add(24*time.Hour)),
		field("0.162.0", "logs/codex.api_request", "product_sku", "unclassified", day2.Add(24*time.Hour)),
		field("0.162.0", "logs/codex.api_request", "turn_count", "unclassified", day2.Add(24*time.Hour), e2e.KindNumber),
	})}
	if err := run(c, day2.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var next Drift
	data, _ = os.ReadFile(filepath.Join(dir, "report", "drift.json"))
	if err := json.Unmarshal(data, &next); err != nil {
		t.Fatal(err)
	}
	if n := next.Harnesses[0]; len(n.Withheld) != 0 || !slices.Equal(keys(n.StillWithheld), []string{"product_sku"}) || !next.Quiet() {
		t.Errorf("the next night: withheld %v, still %v, quiet %v", keys(n.Withheld), keys(n.StillWithheld), next.Quiet())
	}
	if !slices.Equal(h.Unseen, []string{"metrics/codex.turn.e2e_duration_ms"}) {
		t.Errorf("unseen %v", h.Unseen)
	}
	var cat Catalog
	data, _ = os.ReadFile(c.catalog)
	if err := json.Unmarshal(data, &cat); err != nil {
		t.Fatal(err)
	}
	for _, f := range cat.Fields {
		if f.Key == "model" && !slices.Equal(f.Versions, []string{"0.162.0", "0.161.0"}) {
			t.Errorf("model seen in %v, want both builds, newest first", f.Versions)
		}
	}
	md, _ := os.ReadFile(c.fieldsMD)
	for _, want := range []string{"## Codex CLI", "`product_sku` | **unclassified · withheld**", "`old_key` | safe | text | 0.161.0 | **no** (last 0.161.0)"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("FIELDS.md lacks %q:\n%s", want, md)
		}
	}
}

// The Slack message says each harness's changes, links the run, and on a quiet day says so
// in one line rather than staying silent.
func TestSlackDigest(t *testing.T) {
	at := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	busy := Drift{GeneratedAt: at, Link: "https://ci/run/2", Harnesses: []HarnessDrift{{Harness: "claude", Name: "Claude Code", Version: "2.1.296", Previous: "2.1.295",
		Added: []FieldChange{{Surface: "logs/permission_mode_changed", Key: "to_mode", Class: "safe"}}}},
		Compat: []CompatChange{{Harness: "claude", Version: "2.1.296", Platform: "darwin/arm64", Capability: "relay.telemetry", From: "pass", To: "fail"}}}
	data, _ := json.Marshal(busy.slack())
	for _, want := range []string{"Claude Code 2.1.296 (was 2.1.295): 1 new field", "`to_mode` on `logs/permission_mode_changed`", "pass → fail", "https://ci/run/2"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("slack payload lacks %q: %s", want, data)
		}
	}
	quiet := Drift{GeneratedAt: at, Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.162.0"},
		{Harness: "gemini", Name: "Gemini CLI", Version: "0.64.0", Previous: "0.63.0"}}}
	data, _ = json.Marshal(quiet.slack())
	if !quiet.Quiet() || !strings.Contains(string(data), "No changes. Censused: Codex CLI 0.162.0, Gemini CLI 0.64.0 (new build, was 0.63.0).") {
		t.Errorf("quiet day: %s", data)
	}
}
