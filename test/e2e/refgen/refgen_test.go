package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

func TestNormalize(t *testing.T) {
	p := e2e.Profile{
		Harness: "claude", Version: "1.0.0",
		Placeholders: map[string]string{"/var/folders/xy/T/Test1/001/repo": "<repo>", "/var/folders/xy/T/Test1/001": "<sandbox>", "Say hi. TERMA_PROFILE": "<prompt>"},
		Requests: []e2e.ProviderRequest{{Probe: "first-turn", Method: "POST", Path: "/v1/messages",
			Header: map[string][]string{"anthropic-beta": {"prompt-caching-scope-2026-01-05"}, "user-agent": {"codex_exec/0.1 (Mac OS 26.5.1; arm64)"}},
			Body:   json.RawMessage(`{"model":"claude-haiku-4-5-20251001","system":"cwd /private/var/folders/xy/T/Test1/001/repo, home /var/folders/xy/T/Test1/001/home. Today is 2026-11-04, Wednesday, November 4, 2026 at 2026-11-04T04:41:09Z in Europe/Madrid. id 4d2b7fd5-e813-4ae3-b573-50692bf90f1c, url http://127.0.0.1:53021/v1","messages":[{"role":"user","content":"Say hi. TERMA_PROFILE"}]}`)}},
	}
	n := normalize(p)
	body := string(n.Requests[0].Body)
	for _, want := range []string{"cwd <repo>", "home <sandbox>/home", "Today is <date>, <date> at <timestamp> in <timezone>", "id <uuid>", "http://<local>/v1", `"content":"<prompt>"`, "claude-haiku-4-5-20251001"} {
		if !strings.Contains(body, want) {
			t.Errorf("normalized body lacks %q:\n%s", want, body)
		}
	}
	if got := n.Requests[0].Header["anthropic-beta"][0]; got != "prompt-caching-scope-2026-01-05" {
		t.Errorf("a date inside a beta's name was replaced: %s", got)
	}
	if got := n.Requests[0].Header["user-agent"][0]; got != "codex_exec/0.1 (Mac OS <os-release>; arm64)" {
		t.Errorf("user agent = %s", got)
	}
	if n.Placeholders != nil {
		t.Error("placeholders kept after use")
	}
}

func claudeProfile(version, system string, tools []string, betas string, ttl string) e2e.Profile {
	var ts []map[string]any
	for _, name := range tools {
		ts = append(ts, map[string]any{"name": name, "description": name + " does things.", "input_schema": map[string]any{"type": "object"}})
	}
	cache := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		cache["ttl"] = ttl
	}
	b, _ := json.Marshal(map[string]any{"model": "claude-x", "max_tokens": 32000, "thinking": map[string]any{"type": "adaptive"},
		"system": []any{map[string]any{"type": "text", "text": "You are a CLI."}, map[string]any{"type": "text", "text": system, "cache_control": cache}},
		"tools":  ts, "metadata": map[string]any{"user_id": "<hex>"}, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>ctx</system-reminder>"}, map[string]any{"type": "text", "text": "<prompt>"}}}}})
	at := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	return e2e.Profile{Harness: "claude", Version: version, Scenario: "TestClaudeProfile", Platform: "darwin/arm64",
		Command: []string{"claude", "-p"},
		Requests: []e2e.ProviderRequest{
			{Probe: "first-turn", Method: "HEAD", Path: "/api/hello", At: at},
			{Probe: "first-turn", Method: "POST", Path: "/v1/messages", Query: "beta=true", At: at.Add(time.Second), Status: 200,
				Header: map[string][]string{"anthropic-beta": {betas}, "user-agent": {"claude-cli/" + version}, "x-api-key": {"[redacted]"}, "x-claude-code-session-id": {"<uuid>"}},
				Body:   b},
			{Probe: "retries-529", Method: "POST", Path: "/v1/messages", At: at.Add(2 * time.Second), Status: 529, Body: b},
			{Probe: "retries-529", Method: "POST", Path: "/v1/messages", At: at.Add(4500 * time.Millisecond), Status: 200, Body: b},
		},
		Help: map[string]string{"--help": "Usage: claude [options]\n\nOptions:\n  -p, --print  Print\n  --model <m>  Model\n\nCommands:\n  mcp     Configure MCP\n  update  Update\n"},
	}
}

func TestSnapshotFromProfile(t *testing.T) {
	n := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("1.0.0", "Be brief.\nUse tools.", []string{"Bash", "Read"}, "a-2025,b-2026", "")},
		fields:  []e2e.FieldRow{{Harness: "claude", Version: "1.0.0", Surface: "logs/api_request", Key: "cost_usd", Kinds: []string{"number"}, Class: "safe", Platform: "darwin/arm64"}},
		history: []HistoryEntry{{Harness: "claude", Version: "1.0.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"}},
		labels:  map[string]string{"relay.telemetry": "Telemetry reaches the project"}}
	s := n.tonight(buildID{"claude", "1.0.0"})
	if s.Model["default"] != "claude-x" || s.Model["max_tokens"] != float64(32000) {
		t.Errorf("model = %v", s.Model)
	}
	if _, ok := s.Model["params"].(map[string]any)["thinking"]; !ok {
		t.Errorf("thinking not among the params: %v", s.Model["params"])
	}
	if sys := s.Prompt["system"].([]any); len(sys) != 2 || sys[1].(map[string]any)["cache"].(map[string]any)["ttl"] != "5m" {
		t.Errorf("system = %v", sys)
	}
	if ctx := s.Prompt["context"].([]any); len(ctx) != 1 {
		t.Errorf("the scenario's own prompt counted as context: %v", ctx)
	}
	if len(s.Tools) != 2 || s.Tools[0].(map[string]any)["name"] != "Bash" {
		t.Errorf("tools = %v", s.Tools)
	}
	if bps := s.Caching["breakpoints"].([]any); len(bps) != 1 || bps[0].(map[string]any)["at"] != "system[1]" {
		t.Errorf("breakpoints = %v", bps)
	}
	req := s.Request
	if req["endpoint"] != "POST /v1/messages" || strings.Join(req["betas"].([]string), ",") != "a-2025,b-2026" {
		t.Errorf("request = %v", req)
	}
	ids := strings.Join(req["identifiers"].([]string), ",")
	if !strings.Contains(ids, "header:x-claude-code-session-id") || !strings.Contains(ids, "metadata.user_id") {
		t.Errorf("identifiers = %s", ids)
	}
	if len(s.SideRequests) != 1 || s.SideRequests[0].(map[string]any)["path"] != "/api/hello" {
		t.Errorf("side requests = %v", s.SideRequests)
	}
	r := s.Behavior["retries"].(map[string]any)["statuses"].(map[string]any)["529"].(map[string]any)
	if r["attempts"] != 2 || r["honors_retry_after"] != true || r["recovered"] != true {
		t.Errorf("retries = %v", r)
	}
	if strings.Join(s.CLI["flags"].([]string), ",") != "--model,--print" || strings.Join(s.CLI["subcommands"].([]string), ",") != "mcp,update" {
		t.Errorf("cli = %v %v", s.CLI["flags"], s.CLI["subcommands"])
	}
	if s.Telemetry["surfaces"].(map[string]any)["logs/api_request"] == nil {
		t.Errorf("telemetry = %v", s.Telemetry)
	}
	if s.Support["status"] != "supported" {
		t.Errorf("support = %v", s.Support)
	}
}

func changeByID(d *Diff, id string) *Change {
	for i := range d.Changes {
		if d.Changes[i].ID == id {
			return &d.Changes[i]
		}
	}
	return nil
}

func TestDiffClassifies(t *testing.T) {
	a := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("1.0.0", "Be brief.\nUse tools.\nNever guess.", []string{"Bash", "Read"}, "a-2025", "")},
		history: []HistoryEntry{{Harness: "claude", Version: "1.0.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"}},
		fields:  []e2e.FieldRow{{Harness: "claude", Version: "1.0.0", Surface: "logs/api_request", Key: "cost_usd", Kinds: []string{"number"}, Class: "safe"}}}
	b := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("1.0.1", "Be brief.\nUse tools well.\nNever guess.", []string{"Bash", "Monitor"}, "a-2025,c-2026", "1h")},
		history: []HistoryEntry{{Harness: "claude", Version: "1.0.1", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "fail"}},
		fields: []e2e.FieldRow{{Harness: "claude", Version: "1.0.1", Surface: "logs/api_request", Key: "cost_usd", Kinds: []string{"number"}, Class: "safe"},
			{Harness: "claude", Version: "1.0.1", Surface: "logs/api_request", Key: "speed", Kinds: []string{"text"}, Class: "safe"}}}
	sa, sb := a.tonight(buildID{"claude", "1.0.0"}), b.tonight(buildID{"claude", "1.0.1"})
	d := newDiff(sa, sb, &ChangelogNote{Text: "- Added the Monitor tool"}, time.Now())

	if c := changeByID(d, "prompt.system.1.text"); c == nil || c.TextDiff == nil || !strings.Contains(*c.TextDiff, "-Use tools.\n+Use tools well.") || c.Impact != "notable" {
		t.Errorf("system prompt change = %+v", c)
	} else if c.Title != "System prompt changed (+15 −10 chars)" || *c.Documented {
		t.Errorf("system prompt change title %q documented %v", c.Title, *c.Documented)
	}
	if c := changeByID(d, "tools.Monitor"); c == nil || c.Op != "added" || c.Title != "New tool: Monitor" || !*c.Documented {
		t.Errorf("new tool = %+v", c)
	}
	if c := changeByID(d, "tools.Read"); c == nil || c.Op != "removed" || *c.Documented {
		t.Errorf("removed tool = %+v", c)
	}
	if c := changeByID(d, "request.betas.c-2026"); c == nil || c.Title != "New beta: c-2026" {
		t.Errorf("new beta = %+v", c)
	}
	if c := changeByID(d, "caching.breakpoints.system[1].ttl"); c == nil || c.Before != "5m" || c.After != "1h" {
		t.Errorf("ttl change = %+v", c)
	}
	if c := changeByID(d, "telemetry.surfaces.logs/api_request.keys.speed"); c == nil || c.Title != "New telemetry field: speed on logs/api_request" {
		t.Errorf("new field = %+v", c)
	}
	if c := changeByID(d, "support.capabilities.relay.telemetry@darwin/arm64.result"); c == nil || c.Impact != "breaking" || c.Terma == nil || c.Terma.Capabilities[0] != "relay.telemetry" {
		t.Errorf("capability change = %+v", c)
	}
	if c := changeByID(d, "support.status"); c == nil || c.After != "unsupported" {
		t.Errorf("status change = %+v", c)
	}
	if d.Summary.Impact["breaking"] < 1 || d.Summary.Undocumented == nil || *d.Summary.Undocumented == 0 {
		t.Errorf("summary = %+v", d.Summary)
	}
	for _, c := range d.Changes {
		if c.Category == "capture" || strings.HasSuffix(c.ID, "sha256") || strings.HasSuffix(c.ID, ".chars") {
			t.Errorf("a derived or capture field read as a change: %s", c.ID)
		}
	}
}

// A section only one snapshot has is not compared: an older build captured before a capture
// existed did not lack what the newer one shows.
func TestDiffSkipsSectionsOnlyOneHas(t *testing.T) {
	a := &Snapshot{Harness: "codex", Version: "0.1.0", Support: map[string]any{"status": "supported"}}
	b := &Snapshot{Harness: "codex", Version: "0.1.1", Support: map[string]any{"status": "supported"}, Tools: []any{map[string]any{"name": "shell"}}}
	if d := newDiff(a, b, nil, time.Now()); len(d.Changes) != 0 || d.Summary.Undocumented != nil {
		t.Errorf("changes = %+v", d.Changes)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\ntwo\nTHREE\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n"
	d, stats := unifiedDiff(a, b)
	want := "@@ -1,6 +1,6 @@\n one\n two\n-three\n+THREE\n four\n five\n six\n@@ -8,4 +8,5 @@\n eight\n nine\n ten\n+eleven\n \n"
	if d != want {
		t.Errorf("diff =\n%s\nwant\n%s", d, want)
	}
	if stats.AddedChars != 11 || stats.RemovedChars != 5 {
		t.Errorf("stats = %+v", stats)
	}
	long := strings.Repeat("A sentence here. ", 30)
	d, _ = unifiedDiff(long, strings.Replace(long, "A sentence here. A", "A sentence there. A", 1))
	if strings.Count(d, "\n-") != 1 || strings.Count(d, "\n+") != 1 {
		t.Errorf("a long line compared whole:\n%s", d)
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"2.1.9", "2.1.10"}, {"0.2.1-rc.5", "0.2.1"}, {"2026.9.21", "2026.9.24"}, {"0.159.0", "0.159.1"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("%s < %s not said", c[0], c[1])
		}
	}
}

func TestChangelogSections(t *testing.T) {
	md := "# Changelog\n\n## 2.1.318\n\n- Added Monitor\n\n## 2.1.317\n\n- Fixed a bug\n"
	s := changelogSections(md)
	if s["2.1.318"] != "- Added Monitor" || s["2.1.317"] != "- Fixed a bug" {
		t.Errorf("sections = %q", s)
	}
}

// Two nights: the first publishes one build, the second a newer one; the diff between them
// is written, the first build's snapshot kept, the indexes say both.
func TestPublishTwoNights(t *testing.T) {
	dir := t.TempDir()
	first := night{now: time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "a-2025", "")},
		history: []HistoryEntry{{Harness: "claude", Version: "1.0.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"}}}
	if err := publish(dir, first, options{}); err != nil {
		t.Fatal(err)
	}
	second := night{now: time.Date(2026, 11, 4, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{claudeProfile("1.0.1", "Be briefer.", []string{"Bash", "Monitor"}, "a-2025", "")},
		history: []HistoryEntry{{Harness: "claude", Version: "1.0.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"},
			{Harness: "claude", Version: "1.0.1", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"}}}
	if err := publish(dir, second, options{}); err != nil {
		t.Fatal(err)
	}
	var idx Index
	if err := readJSON(filepath.Join(dir, "index.json"), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Harnesses) != 1 || idx.Harnesses[0].Latest != "1.0.1" || idx.Harnesses[0].VersionsTracked != 2 || idx.Harnesses[0].LastChange == nil {
		t.Fatalf("index = %+v", idx)
	}
	var hi HarnessIndex
	if err := readJSON(filepath.Join(dir, "claude", "index.json"), &hi); err != nil {
		t.Fatal(err)
	}
	if hi.Versions[0].Version != "1.0.1" || *hi.Versions[0].Previous != "1.0.0" || hi.Versions[0].Changes == 0 || len(hi.Versions[0].Highlights) == 0 {
		t.Errorf("versions = %+v", hi.Versions)
	}
	var old Snapshot
	if err := readJSON(filepath.Join(dir, "claude", "versions", "1.0.0.json"), &old); err != nil {
		t.Fatal(err)
	}
	if !old.CapturedAt.Equal(first.now) || old.Prompt == nil {
		t.Errorf("the first night's snapshot was not kept: captured %v", old.CapturedAt)
	}
	var d Diff
	if err := readJSON(filepath.Join(dir, "claude", "diffs", "1.0.0..1.0.1.json"), &d); err != nil {
		t.Fatal(err)
	}
	if changeByID(&d, "tools.Monitor") == nil {
		t.Errorf("diff = %+v", d.Changes)
	}
	md, err := os.ReadFile(filepath.Join(dir, "claude", "prompts", "1.0.1.md"))
	if err != nil || !strings.Contains(string(md), "Be briefer.") {
		t.Errorf("prompt markdown = %s, %v", md, err)
	}
	// A build arriving between two published ones replaces their diff with two.
	third := night{now: time.Date(2026, 11, 5, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{claudeProfile("1.0.0-rc.1", "Be brief!", []string{"Bash"}, "a-2025", "")}}
	third.profiles[0].Version = "1.0.1-rc.1"
	if err := publish(dir, third, options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude", "diffs", "1.0.0..1.0.1.json")); err == nil {
		t.Error("a diff between builds no longer neighbours was kept")
	}
	for _, f := range []string{"1.0.0..1.0.1-rc.1.json", "1.0.1-rc.1..1.0.1.json"} {
		if _, err := os.Stat(filepath.Join(dir, "claude", "diffs", f)); err != nil {
			t.Errorf("diff %s: %v", f, err)
		}
	}
}
