package main

import (
	"encoding/json"
	"fmt"
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
	r := e2e.FieldRow{Harness: "codex", Version: version, Surface: surface, Key: key, Class: class, Kinds: kinds, Platform: "darwin/arm64", At: at}
	if class == "unclassified" {
		// What `terma relay classify` keeps of an unclassified key on a record.
		r.Kept = []string{e2e.KindNumber, e2e.KindBool}
	}
	return r
}

// A run's census against the catalog: a key no build had is new, a key the previous newest
// build had on a surface still seen is removed, a surface not seen is said apart, and an
// unclassified key with text values is withheld while one with only numbers passes, on a
// record; on a resource the relay withholds it whatever its value. The catalog then has every
// build each key was seen in.
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
	// `terma relay classify` keeps no kind of value of an unclassified resource attribute.
	pid := func(at time.Time) e2e.FieldRow {
		r := field("0.162.0", "resource", "process.parent_pid", "unclassified", at, e2e.KindNumber)
		r.Kept = nil
		return r
	}
	c.fieldRuns = []string{writeFieldRun(t, dir, "f2.json", []e2e.FieldRow{
		pid(day2),
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
	if got := keys(h.Added); !slices.Equal(got, []string{"product_sku", "turn_count", "process.parent_pid"}) {
		t.Errorf("added %v", got)
	}
	if got := keys(h.Removed); !slices.Equal(got, []string{"old_key"}) {
		t.Errorf("removed %v", got)
	}
	if got := keys(h.Withheld); !slices.Equal(got, []string{"product_sku", "process.parent_pid"}) {
		t.Errorf("withheld %v: an unclassified number passes on a record, not on a resource", got)
	}
	// The next night, the same key withheld is a reminder, not a change.
	c.fieldRuns = []string{writeFieldRun(t, dir, "f3.json", []e2e.FieldRow{
		pid(day2.Add(24 * time.Hour)),
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
	if n := next.Harnesses[0]; len(n.Withheld) != 0 || !slices.Equal(keys(n.StillWithheld), []string{"product_sku", "process.parent_pid"}) || !next.Quiet() {
		t.Errorf("the next night: withheld %v, still %v, quiet %v", keys(n.Withheld), keys(n.StillWithheld), next.Quiet())
	}
	if len(h.Unseen) != 1 || h.Unseen[0].Surface != "metrics/codex.turn.e2e_duration_ms" {
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
	for _, want := range []string{"## Codex CLI", "`product_sku` | **unclassified · withheld**", "`process.parent_pid` | **unclassified · withheld** | number", "`old_key` | safe | text | 0.161.0 | **no** (last 0.161.0)"} {
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

// A night that brought no census, or none of a harness the catalog censused within the week,
// is no quiet night: a census that did not run must not read as one that found nothing.
func TestDigestSaysWhatWasNotCensused(t *testing.T) {
	now := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	cat := Catalog{Censuses: []Census{
		{Harness: "codex", Version: "0.162.0", At: now.Add(-24 * time.Hour)},
		{Harness: "hermes", Version: "0.20.5", At: now.Add(-30 * 24 * time.Hour)}, // aged out
	}}
	noCensus, missing, _ := fieldDrift(cat, nil, nil, now)
	if d := (Drift{NoCensus: noCensus, Missing: missing}); !noCensus || d.Quiet() {
		t.Errorf("no rows: noCensus %v, quiet %v", noCensus, d.Quiet())
	}
	rows := []e2e.FieldRow{field("2.1.296", "logs/api_request", "model", "safe", now)}
	rows[0].Harness = "claude"
	noCensus, missing, hs := fieldDrift(cat, rows, nil, now)
	d := Drift{NoCensus: noCensus, Missing: missing, Harnesses: hs}
	if noCensus || !slices.Equal(missing, []string{"Codex CLI"}) || d.Quiet() {
		t.Errorf("claude only: noCensus %v, missing %v, quiet %v", noCensus, missing, d.Quiet())
	}
	if !strings.Contains(d.markdown(), "No census this night of Codex CLI") {
		t.Errorf("the digest does not say Codex was not censused:\n%s", d.markdown())
	}
}

// A surface no build had is said once, with its keys counted, not key by key: a span renamed
// carries keys the harness already sends. A key new to the harness on it is still a new
// field. A re-run of the same build reports nothing gone: a key that only comes on an error
// path would otherwise read as removed.
func TestDigestSurfacesAndReruns(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.162.0", "traces/old_fn", "code.file.path", "safe", day),
		field("0.162.0", "traces/old_fn", "busy_ns", "safe", day, e2e.KindNumber),
		field("0.162.0", "logs/codex.tool_result", "error", "prompt", day), // only on an error path
	})
	tonight := []e2e.FieldRow{
		field("0.162.0", "traces/new_fn", "code.file.path", "safe", day.Add(24*time.Hour)),
		field("0.162.0", "traces/new_fn", "busy_ns", "safe", day.Add(24*time.Hour), e2e.KindNumber),
		field("0.162.0", "traces/new_fn", "fresh_key", "safe", day.Add(24*time.Hour)),
	}
	_, _, hs := fieldDrift(cat, tonight, nil, day.Add(24*time.Hour))
	h := hs[0]
	if len(h.NewSurfaces) != 1 || h.NewSurfaces[0] != (SurfaceChange{Surface: "traces/new_fn", Keys: 3, NewKeys: 1}) {
		t.Errorf("new surfaces %+v", h.NewSurfaces)
	}
	if len(h.Added) != 1 || h.Added[0].Key != "fresh_key" {
		t.Errorf("added %+v: only the key new to the harness", h.Added)
	}
	if len(h.Removed)+len(h.Unseen) != 0 {
		t.Errorf("a re-run of the same build reported removed %+v, unseen %v", h.Removed, h.Unseen)
	}
}

// A key keeps the first build it was seen in and its newest keepVersions; a harness its
// newest keepCensuses censuses; and the catalog is written one entry a line.
func TestCatalogIsBounded(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	for i := range 8 {
		v := fmt.Sprintf("0.16%d.0", i)
		mergeFields(&cat, []e2e.FieldRow{field(v, "logs/codex.api_request", "model", "safe", day.Add(time.Duration(i)*24*time.Hour))})
	}
	f := cat.Fields[0]
	if f.FirstSeen != "0.160.0" || !slices.Equal(f.Versions, []string{"0.167.0", "0.166.0", "0.165.0", "0.164.0", "0.163.0"}) {
		t.Errorf("first seen %s, versions %v", f.FirstSeen, f.Versions)
	}
	if len(cat.Censuses) != keepCensuses || cat.Censuses[0].Version != "0.167.0" {
		t.Errorf("censuses %+v", cat.Censuses)
	}
	path := filepath.Join(t.TempDir(), "fields.json")
	if err := writeCatalog(path, cat); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var back Catalog
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	// {, generated_at, "censuses": [, an entry a line, ], "fields": [, an entry a line, ], }
	if lines := strings.Count(string(data), "\n"); lines != 3+keepCensuses+2+len(cat.Fields)+2 {
		t.Errorf("%d lines, want one per entry:\n%s", lines, data)
	}
	if len(back.Fields) != 1 || len(back.Censuses) != keepCensuses {
		t.Errorf("read back %+v", back)
	}
}

// A Slack section is cut at the end of a line, so neither a character, a code span nor an
// escape is split.
func TestClip(t *testing.T) {
	s := "*Codex*\n• `ключ` on `traces/x`\n• `ключ2` on `traces/y`"
	got := clip(s, len("*Codex*\n• `ключ` on `traces/x`\n• `кл"))
	if got != "*Codex*\n• `ключ` on `traces/x`\n…" {
		t.Errorf("clip = %q", got)
	}
	if clip(s, 1000) != s {
		t.Error("clip cut a short text")
	}
	long := strings.Repeat("ключ", 10)
	if got := clip(long, 5); got != "кл\n…" {
		t.Errorf("clip with no line end = %q", got)
	}
	if got := clip(slackEscape("a<b&c"), len("a&lt;b&am")); got != "a&lt;b\n…" {
		t.Errorf("clip inside an escape = %q", got)
	}
}

// A key every kept census has aged past leaves the catalog, and a key's kinds and class are
// its newest build's: a key an old build sent as text and a new one as a number is no longer
// withheld.
func TestCatalogKeepsWhatItsCensusesSaw(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{
		field("0.150.0", "logs/codex.api_request", "gone_key", "safe", day),
		field("0.150.0", "logs/codex.api_request", "turns", "unclassified", day),
	})
	for i := range keepCensuses {
		v := fmt.Sprintf("0.16%d.0", i)
		mergeFields(&cat, []e2e.FieldRow{field(v, "logs/codex.api_request", "turns", "unclassified", day.Add(time.Duration(i+1)*24*time.Hour), e2e.KindNumber)})
	}
	keys := map[string]FieldEntry{}
	for _, f := range cat.Fields {
		keys[f.Key] = f
	}
	if _, ok := keys["gone_key"]; ok {
		t.Error("a key no kept census saw is still in the catalog")
	}
	if f := keys["turns"]; !slices.Equal(f.Kinds, []string{e2e.KindNumber}) || f.withheld() {
		t.Errorf("turns: kinds %v, withheld %v: the newest build sends a number", f.Kinds, f.withheld())
	}
}

// A key withheld one night and classified by the next is no longer withheld, though the
// harness shipped no new build: the class, and what the relay keeps, are the latest run's.
func TestCatalogTakesTheLatestClassOfABuild(t *testing.T) {
	day := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "product_sku", "unclassified", day)})
	if len(cat.Fields) != 1 || !cat.Fields[0].withheld() {
		t.Fatalf("night 1: %+v, want product_sku withheld", cat.Fields)
	}
	mergeFields(&cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "product_sku", "safe", day.Add(24*time.Hour))})
	if f := cat.Fields[0]; f.Class != "safe" || f.withheld() {
		t.Errorf("night 2: class %s, withheld %v, want safe and sent", f.Class, f.withheld())
	}
	if md := renderFields(cat, day.Add(24*time.Hour)); strings.Contains(md, "withheld**") || !strings.Contains(md, "| `product_sku` | safe |") {
		t.Errorf("FIELDS.md still has product_sku withheld:\n%s", md)
	}
}

// A night that reached only a build older than the catalog's newest says so, and is no quiet
// night: the newest failed to install, or its tests did not run.
func TestDigestSaysTheNewestWasNotReached(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.162.1", "logs/codex.api_request", "model", "safe", day)})
	_, _, hs := fieldDrift(cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", day.Add(24*time.Hour))}, nil, day.Add(24*time.Hour))
	d := Drift{Harnesses: hs}
	if hs[0].Unreached != "0.162.1" || d.Quiet() || !strings.Contains(hs[0].headline(), "0.162.1, the newest censused before, was not reached") {
		t.Errorf("unreached %q, quiet %v, headline %q", hs[0].Unreached, d.Quiet(), hs[0].headline())
	}
}

// A night whose census scenarios ran a newer build than any census reached says so, though
// the build the census did reach is newer than the catalog's: the newest failed before its
// census was taken.
func TestDigestSaysTheNewestRunWasNotReached(t *testing.T) {
	day := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	var cat Catalog
	mergeFields(&cat, []e2e.FieldRow{field("0.160.0", "logs/codex.api_request", "model", "safe", day)})
	night := day.Add(24 * time.Hour)
	ran := censusRan([]e2e.CompatRow{
		{Harness: "codex", Version: "0.161.0", Capability: e2e.CensusCapability, Result: "pass"},
		{Harness: "codex", Version: "0.162.0", Capability: e2e.CensusCapability, Result: "fail"},
		// Neither a skipped scenario nor another capability's is a census that should have been taken.
		{Harness: "codex", Version: "0.163.0", Capability: e2e.CensusCapability, Result: "not run"},
		{Harness: "codex", Version: "0.164.0", Capability: "relay.telemetry", Result: "fail"},
	})
	_, _, hs := fieldDrift(cat, []e2e.FieldRow{field("0.161.0", "logs/codex.api_request", "model", "safe", night)}, ran, night)
	d := Drift{Harnesses: hs}
	if hs[0].Unreached != "0.162.0" || d.Quiet() || !strings.Contains(hs[0].headline(), "0.161.0 (0.162.0, run tonight, was not reached)") {
		t.Errorf("unreached %q, quiet %v, headline %q", hs[0].Unreached, d.Quiet(), hs[0].headline())
	}
	// The census of the newest build it ran: nothing unreached.
	_, _, hs = fieldDrift(cat, []e2e.FieldRow{field("0.162.0", "logs/codex.api_request", "model", "safe", night)}, ran, night)
	if hs[0].Unreached != "" || !(Drift{Harnesses: hs}).Quiet() {
		t.Errorf("unreached %q with the newest censused", hs[0].Unreached)
	}
}

// What a harness names is escaped where it is shown: "<" in Slack would start a link, and "|"
// in a markdown table would end a cell.
func TestDigestEscapesWhatHarnessesName(t *testing.T) {
	d := Drift{GeneratedAt: time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC), Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "1", Previous: "1",
		Added: []FieldChange{{Surface: "logs/<x|y>", Key: "a&b"}}}}}
	data, _ := json.Marshal(d.slack())
	if !strings.Contains(string(data), "a\\u0026amp;b") || !strings.Contains(string(data), "\\u0026lt;x|y\\u0026gt;") {
		t.Errorf("slack text not escaped: %s", data)
	}
	if got := tableCell("a|b"); got != `a\|b` {
		t.Errorf("tableCell = %q", got)
	}
}

// A night that changed everything is still one message Slack takes: at most 50 blocks, the
// sections' text within the budget, and a line saying what was left for the run's summary.
// Many small changes meet the block limit first, a few long ones the budget.
func TestSlackDigestFitsOneMessage(t *testing.T) {
	for _, c := range []struct {
		name            string
		harnesses, keys int
		blocksFull      bool
	}{
		{"many short", 80, 1, true},
		{"few long", 20, 40, false},
	} {
		d := Drift{GeneratedAt: time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC), Link: "https://ci/run/3"}
		for i := range c.harnesses {
			h := HarnessDrift{Harness: fmt.Sprint("h", i), Name: fmt.Sprint("Harness ", i), Version: "2", Previous: "1"}
			for k := range c.keys {
				f := FieldChange{Surface: "logs/" + strings.Repeat("s", 150), Key: fmt.Sprint("key_", k)}
				h.Added, h.Removed = append(h.Added, f), append(h.Removed, f)
			}
			d.Harnesses = append(d.Harnesses, h)
		}
		payload := d.slack()
		blocks := payload["blocks"].([]map[string]any)
		size := 0
		for _, b := range blocks {
			if b["type"] == "section" {
				size += len(b["text"].(map[string]any)["text"].(string))
			}
		}
		if len(blocks) > maxBlocks || size > slackBudget+sectionLimit || (len(blocks) == maxBlocks) != c.blocksFull {
			t.Errorf("%s: %d blocks, %d characters of section text", c.name, len(blocks), size)
		}
		data, _ := json.Marshal(payload)
		for _, want := range []string{"more, too long for one message", "https://ci/run/3", "Harness 0 2 (was 1)"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s: slack payload lacks %q", c.name, want)
			}
		}
	}
}

// A headline counts each change in its own plural: "surfaces no longer sent", not "surface no
// longer sents".
func TestHeadlinePlurals(t *testing.T) {
	h := HarnessDrift{Name: "Codex CLI", Version: "0.162.1", Previous: "0.162.0", Unseen: []GoneSurface{{Surface: "a"}, {Surface: "b"}},
		Added: []FieldChange{{Key: "k"}}, Withheld: []FieldChange{{Key: "k"}, {Key: "l"}}}
	if got, want := h.headline(), "Codex CLI 0.162.1 (was 0.162.0): 1 new field, 2 surfaces no longer sent, 2 newly withheld fields"; got != want {
		t.Errorf("headline = %q, want %q", got, want)
	}
}
