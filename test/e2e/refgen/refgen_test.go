package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/refdiff"
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
	cmds := s.CLI["commands"].(map[string]any)
	opts := cmds["claude"].(map[string]any)["options"].(map[string]any)
	if opts["--print"] == nil || opts["--model"] == nil || cmds["claude mcp"] == nil || cmds["claude update"] == nil {
		t.Errorf("cli commands = %v", cmds)
	}
	if s.Telemetry["surfaces"].(map[string]any)["logs/api_request"] == nil {
		t.Errorf("telemetry = %v", s.Telemetry)
	}
	if s.Support == nil || s.Support.Status != "supported" {
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
	d := refdiff.New(sa, sb, &ChangelogNote{Text: "- Added the Monitor tool"}, time.Now())

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
	a := &Snapshot{Harness: "codex", Version: "0.1.0", Support: &refdiff.Support{Status: "supported"}}
	b := &Snapshot{Harness: "codex", Version: "0.1.1", Support: &refdiff.Support{Status: "supported"}, Tools: []any{map[string]any{"name": "shell"}}}
	if d := refdiff.New(a, b, nil, time.Now()); len(d.Changes) != 0 || d.Summary.Undocumented != nil {
		t.Errorf("changes = %+v", d.Changes)
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
	if _, err := publish(dir, first, options{}); err != nil {
		t.Fatal(err)
	}
	second := night{now: time.Date(2026, 11, 4, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{claudeProfile("1.0.1", "Be briefer.", []string{"Bash", "Monitor"}, "a-2025", "")},
		history: []HistoryEntry{{Harness: "claude", Version: "1.0.0", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"},
			{Harness: "claude", Version: "1.0.1", Platform: "darwin/arm64", Capability: "relay.telemetry", Result: "pass"}}}
	if _, err := publish(dir, second, options{}); err != nil {
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
	if _, err := publish(dir, third, options{}); err != nil {
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

// A build a run set out to capture and captured nothing of is published, partial, so the
// hourly check does not take it for new again; a later capture fills it in.
func TestAttemptedBuildIsPublished(t *testing.T) {
	dir := t.TempDir()
	n := night{now: time.Now(), attempted: []buildID{{"codex", "0.200.0"}}}
	if _, err := publish(dir, n, options{}); err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err := readJSON(filepath.Join(dir, "codex", "versions", "0.200.0.json"), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Capture.Partial || len(s.Capture.Notes) == 0 {
		t.Errorf("capture = %+v", s.Capture)
	}
	later := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("0.200.0", "Be brief.", []string{"shell"}, "", "")}}
	later.profiles[0].Harness = "codex"
	if _, err := publish(dir, later, options{}); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(filepath.Join(dir, "codex", "versions", "0.200.0.json"), &s); err != nil {
		t.Fatal(err)
	}
	if s.Capture.Partial || s.Prompt == nil {
		t.Errorf("a later capture did not fill the attempted build in: %+v", s.Capture)
	}
}

// A build's signature is published with its binary, and its certificate's revocation reads as
// a notable change; when it was checked does not.
func TestSigningChange(t *testing.T) {
	sig := func(status string, at time.Time) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"format": "developer-id", "team_id": "2DC432GLL2", "status": status, "checked_at": at})
		return data
	}
	a := claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "", "")
	a.Probes = map[string]json.RawMessage{"signing": sig("valid", time.Now())}
	b := claudeProfile("1.0.1", "Be brief.", []string{"Bash"}, "", "")
	b.Probes = map[string]json.RawMessage{"signing": sig("revoked", time.Now().Add(time.Hour))}
	sa := night{now: time.Now(), profiles: []e2e.Profile{a}}.tonight(buildID{"claude", "1.0.0"})
	sb := night{now: time.Now(), profiles: []e2e.Profile{b}}.tonight(buildID{"claude", "1.0.1"})
	if sb.Binary["signing"].(map[string]any)["status"] != "revoked" {
		t.Fatalf("binary = %v", sb.Binary)
	}
	d := refdiff.New(sa, sb, nil, time.Now())
	c := changeByID(d, "binary.signing.status")
	if c == nil || c.Impact != "notable" || c.Title != "Signing certificate revoked" {
		t.Errorf("signing change = %+v", c)
	}
	if changeByID(d, "binary.signing.checked_at") != nil {
		t.Error("when the signature was checked read as a change")
	}
}

func TestNeedleOf(t *testing.T) {
	for _, c := range []struct {
		change Change
		want   string
	}{
		{Change{Category: "tools", Path: []string{"tools", "collaboration/spawn_agent", "description"}}, "spawn_agent"},
		{Change{Category: "tools", Path: []string{"tools", "Bash", "input_schema", "properties", "timeout", "description"}}, "timeout"},
		{Change{Category: "telemetry", Path: []string{"telemetry", "surfaces", "logs/api_request", "keys", "speed"}}, "speed"},
		{Change{Category: "telemetry", Path: []string{"telemetry", "surfaces", "metrics/codex.turn.e2e_duration_ms"}}, "codex.turn.e2e_duration_ms"},
		{Change{Category: "model", Path: []string{"model", "default"}, Op: "changed", Before: "gpt-6-astra", After: "gpt-6.1-sol"}, "gpt-6.1-sol"},
		{Change{Category: "request", Path: []string{"request", "betas", "remote_compaction_v2"}}, "remote_compaction_v2"},
		{Change{Category: "side_requests", Path: []string{"side_requests", "POST /analytics/codex/turn-costs"}}, "/analytics/codex/turn-costs"},
		{Change{Category: "cli", Path: []string{"cli", "flags", "exec --json"}}, "--json"},
		{Change{Category: "support", Path: []string{"support", "status"}}, ""},
	} {
		if got := needleOf(&c.change); got != c.want {
			t.Errorf("needleOf(%v) = %q, want %q", c.change.Path, got, c.want)
		}
	}
}

func TestDiffSides(t *testing.T) {
	a, r := diffSides("@@ -1,2 +1,2 @@\n same\n-old line\n+new line\n")
	if a != "new line\n" || r != "old line\n" {
		t.Errorf("sides = %q, %q", a, r)
	}
}

// A version newer than every one captured before is announced, once; an older one arriving
// late (a backfill) and a harness's first publication are not.
func TestAnnouncements(t *testing.T) {
	dir := t.TempDir()
	opt := options{site: "https://terma.ai"}
	first := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "", "")}}
	if news, err := publish(dir, first, opt); err != nil || len(news) != 0 {
		t.Fatalf("first publication announced %v (%v)", news, err)
	}
	second := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("1.0.1", "Be briefer.", []string{"Bash", "Monitor"}, "", "")}}
	news, err := publish(dir, second, opt)
	if err != nil || len(news) != 1 {
		t.Fatalf("announced %+v (%v)", news, err)
	}
	a := news[0]
	if a.Version != "1.0.1" || a.Previous != "1.0.0" || a.URL != "https://terma.ai/h/claude/1.0.1" ||
		a.CompareURL != "https://terma.ai/harnesses/claude/compare/1.0.0...1.0.1" || len(a.Highlights) == 0 {
		t.Errorf("announcement = %+v", a)
	}
	if !strings.HasPrefix(a.Text, "Claude Code 1.0.1 is out.") || !strings.HasSuffix(a.Text, a.URL) || len([]rune(a.Text))-len(a.URL)+23 > 280 {
		t.Errorf("post = %q", a.Text)
	}
	if a.Slack == nil || len(a.Slack.Blocks) == 0 || !strings.Contains(a.Slack.Text, a.URL) {
		t.Errorf("slack = %v", a.Slack)
	}
	if news, _ := publish(dir, second, opt); len(news) != 0 {
		t.Errorf("announced again: %+v", news)
	}
	late := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("0.9.0", "Be.", []string{"Bash"}, "", "")}}
	if news, _ := publish(dir, late, opt); len(news) != 0 {
		t.Errorf("a backfilled version announced: %+v", news)
	}
	var tl Timeline
	if err := readJSON(filepath.Join(dir, "claude", "timeline.json"), &tl); err != nil || len(tl.Steps) != 2 || tl.Steps[0].Version != "1.0.1" {
		t.Errorf("timeline = %+v (%v)", tl, err)
	}
	var cl Changelogs
	if err := readJSON(filepath.Join(dir, "claude", "changelog.json"), &cl); err != nil || cl.Versions == nil {
		t.Errorf("changelog = %+v (%v)", cl, err)
	}
}

// A night that captures a build the same as before moves nothing: its capture time stays, so
// a feed does not read every build as changed tonight.
func TestSameCaptureKeepsItsTime(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	n := night{now: at, profiles: []e2e.Profile{claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "", "")}}
	if _, err := publish(dir, n, options{}); err != nil {
		t.Fatal(err)
	}
	n.now = at.Add(24 * time.Hour)
	if _, err := publish(dir, n, options{}); err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err := readJSON(filepath.Join(dir, "claude", "versions", "1.0.0.json"), &s); err != nil {
		t.Fatal(err)
	}
	if !s.CapturedAt.Equal(at) || !s.Capture.Sections["prompt"].Equal(at) {
		t.Errorf("captured %v, prompt %v; want both %v", s.CapturedAt, s.Capture.Sections["prompt"], at)
	}
	var r Recent
	if err := readJSON(filepath.Join(dir, "recent.json"), &r); err != nil || r.Versions == nil {
		t.Errorf("recent = %+v (%v)", r, err)
	}
}

// A capability tested on a platform for one build alone was not tested on the other there:
// no change. A model named in words in the changelog is documented.
func TestSupportPlatformsAndModelWords(t *testing.T) {
	a := &Snapshot{Harness: "codex", Version: "0.1.0", Model: map[string]any{"default": "gpt-6-astra"},
		Support: &refdiff.Support{Status: "supported", Capabilities: []refdiff.Capability{
			{ID: "relay.telemetry", Platform: "darwin/arm64", Result: "pass", Label: "Telemetry reaches the project"}}}}
	b := &Snapshot{Harness: "codex", Version: "0.1.1", Model: map[string]any{"default": "gpt-6.1-sol"},
		Support: &refdiff.Support{Status: "supported", Capabilities: []refdiff.Capability{
			{ID: "relay.telemetry", Platform: "darwin/arm64", Result: "pass", Label: "Telemetry reaches the project"},
			{ID: "global.commits", Platform: "linux/amd64", Result: "pass", Label: "Commits stamped"}}}}
	d := refdiff.New(a, b, &ChangelogNote{Text: "- Added GPT-6.1 Sol as the default model"}, time.Now())
	if len(d.Changes) != 1 || d.Changes[0].ID != "model.default" || !*d.Changes[0].Documented {
		t.Errorf("changes = %+v", d.Changes)
	}
	b.Support.Capabilities[0].Result = "fail"
	d = refdiff.New(a, b, nil, time.Now())
	if c := changeByID(d, "support.capabilities.relay.telemetry@darwin/arm64.result"); c == nil || c.Title != "Terma: Telemetry reaches the project now fails on darwin/arm64" {
		t.Errorf("capability change = %+v", c)
	}
}

// The proxied turn is the published first turn; the plain one, with live flags, is kept apart
// and never compared.
func TestProxiedFirstTurn(t *testing.T) {
	plain := claudeProfile("1.0.0", "Live flags on.", []string{"Bash"}, "", "")
	proxied := claudeProfile("1.0.0", "Shipped defaults.", []string{"Bash"}, "", "")
	proxied.Scenario = "TestClaudeProfileEgress"
	proxied.Help = nil
	n := night{now: time.Now(), profiles: []e2e.Profile{plain, proxied}}
	s := n.tonight(buildID{"claude", "1.0.0"})
	sys := s.Prompt["system"].([]any)
	if got := sys[1].(map[string]any)["text"]; got != "Shipped defaults." {
		t.Errorf("published prompt = %q", got)
	}
	if s.Live == nil || s.Live["differs"] != true {
		t.Errorf("live = %v", s.Live)
	}
	if s.CLI == nil {
		t.Error("the plain turn's help was lost")
	}
	other := *s
	other.Version = "1.0.1"
	other.Live = map[string]any{"differs": false}
	if d := refdiff.New(s, &other, nil, time.Now()); len(d.Changes) != 0 {
		t.Errorf("live flags read as changes: %+v", d.Changes)
	}
	failed := proxied
	failed.Failed = true
	s = night{now: time.Now(), profiles: []e2e.Profile{plain, failed}}.tonight(buildID{"claude", "1.0.0"})
	if got := s.Prompt["system"].([]any)[1].(map[string]any)["text"]; got != "Live flags on." || s.Live != nil {
		t.Errorf("a failed proxied turn was published: %q, live %v", got, s.Live)
	}
}

// The capture's command is the published turn's, not that of a probe whose scenario sorts first.
func TestCaptureCommandIsTheTurns(t *testing.T) {
	probe := e2e.Profile{Harness: "claude", Version: "1.0.0", Platform: "darwin/arm64", Scenario: "TestClaudeProbeHooks",
		Command: []string{"claude", "--allowedTools", "Bash"}}
	proxied := claudeProfile("1.0.0", "Shipped defaults.", []string{"Bash"}, "", "")
	proxied.Scenario = "TestClaudeProfileEgress"
	proxied.Command = []string{"claude", "--output-format", "json"}
	s := night{now: time.Now(), profiles: []e2e.Profile{probe, proxied}}.tonight(buildID{"claude", "1.0.0"})
	if got := strings.Join(s.Capture.Command, " "); got != "claude --output-format json" {
		t.Errorf("capture command = %q", got)
	}
}

func TestIsSubagent(t *testing.T) {
	meta := func(agent string) map[string]any {
		m, _ := json.Marshal(map[string]string{"agent_name": agent})
		return map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": string(m)}}
	}
	if isSubagent(e2e.ProviderRequest{}, meta("/root")) || !isSubagent(e2e.ProviderRequest{}, meta("/root/check")) {
		t.Error("Codex's root and sub-agent told apart wrongly")
	}
	if !isSubagent(e2e.ProviderRequest{Body: json.RawMessage(`{"system":[{"text":"x-anthropic-billing-header: cc_is_subagent=true;"}]}`)}, nil) {
		t.Error("Claude Code's subagent not told")
	}
}

// A signature's facts survive normalization, and a new certificate is one change; when each
// build was signed is none.
func TestSigningNormalizedAndCompared(t *testing.T) {
	sig := func(serial, signedAt string) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"status": "valid", "signed_at": signedAt, "checked_at": "2026-11-04T04:35:53Z",
			"certificate": map[string]any{"subject": "Developer ID Application: X", "serial": serial,
				"sha256": "9935b17bbc8e5127656097958e229eaa031c972c2e0da530cdb90f354777cbd5", "not_before": "2024-01-23T06:19:53Z"}})
		return data
	}
	a := claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "", "")
	a.Probes = map[string]json.RawMessage{"signing": sig("68A68AF8A434EE4FDE46F8680B23BDCC", "25 Jan 2026 at 17:11:12")}
	b := claudeProfile("1.0.1", "Be brief.", []string{"Bash"}, "", "")
	b.Probes = map[string]json.RawMessage{"signing": sig("1234", "26 Jan 2026 at 10:00:00")}
	sa := night{now: time.Now(), profiles: []e2e.Profile{a}}.tonight(buildID{"claude", "1.0.0"})
	sb := night{now: time.Now(), profiles: []e2e.Profile{b}}.tonight(buildID{"claude", "1.0.1"})
	cert := sa.Binary["signing"].(map[string]any)["certificate"].(map[string]any)
	if cert["sha256"] != "9935b17bbc8e5127656097958e229eaa031c972c2e0da530cdb90f354777cbd5" || cert["not_before"] != "2024-01-23T06:19:53Z" {
		t.Errorf("certificate normalized away: %v", cert)
	}
	d := refdiff.New(sa, sb, nil, time.Now())
	if len(d.Changes) != 1 || d.Changes[0].ID != "binary.signing.certificate" || d.Changes[0].Title != "Signed with a different certificate" {
		t.Errorf("changes = %+v", d.Changes)
	}
}

// A command's -h is read where its --help is missing, and itemized the same way; the full
// texts are compared too.
func TestShortHelp(t *testing.T) {
	p := claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "", "")
	p.Help = map[string]string{"--help": "Usage: codex [OPTIONS]\n\nOptions:\n  --model <M>  Model\n", "-h": "Usage: codex [OPTIONS]\n\nOptions:\n  --model <M>\n",
		"exec -h": "Usage: codex exec [OPTIONS]\n\nOptions:\n  --json  JSONL\n"}
	s := night{now: time.Now(), profiles: []e2e.Profile{p}}.tonight(buildID{"claude", "1.0.0"})
	cmds := s.CLI["commands"].(map[string]any)
	if cmds["codex"].(map[string]any)["options"].(map[string]any)["--model"] == nil ||
		cmds["codex exec"].(map[string]any)["options"].(map[string]any)["--json"] == nil {
		t.Errorf("commands = %v", cmds)
	}
	q := p
	q.Version = "1.0.1"
	q.Help = map[string]string{"--help": "Usage: codex [OPTIONS]\n\nOptions:\n  --model <M>  Model\n  --oss        Local\n", "-h": p.Help["-h"], "exec -h": p.Help["exec -h"]}
	s2 := night{now: time.Now(), profiles: []e2e.Profile{q}}.tonight(buildID{"claude", "1.0.1"})
	d := refdiff.New(s, s2, nil, time.Now())
	if c := changeByID(d, "cli.commands.codex.options.--oss"); c == nil || c.Title != "New flag `codex --oss`" {
		t.Errorf("new flag = %+v", c)
	}
	if c := changeByID(d, "cli.help.--help"); c == nil || !strings.HasPrefix(c.Title, "Full help text of `--help` changed") {
		t.Errorf("help text = %+v", c)
	}
}

func TestLatestProfiles(t *testing.T) {
	a := e2e.Profile{Harness: "claude", Version: "1.0.0", Scenario: "TestClaudeProbeCompaction", Platform: "darwin/arm64", Failed: true}
	b := a
	b.Failed = false
	c := e2e.Profile{Harness: "claude", Version: "1.0.0", Scenario: "TestClaudeProfile", Platform: "darwin/arm64"}
	got := latestProfiles([]e2e.Profile{a, c, b})
	if len(got) != 2 || got[0].Scenario != "TestClaudeProfile" || got[1].Failed {
		t.Errorf("latest = %+v", got)
	}
	// A later pass that failed does not replace an earlier whole one.
	got = latestProfiles([]e2e.Profile{b, c, a})
	if len(got) != 2 || got[0].Failed || got[0].Scenario != "TestClaudeProbeCompaction" {
		t.Errorf("a failed later pass replaced a whole one: %+v", got)
	}
}

const settingsDocMD = "# All settings\n\n## Model and responses\n\n### `model`\n\nThe model to use. See [models](https://x/models.md).\n\n" +
	"* **Scope**: [`Any file`](#scopes)\n* **Type**: string\n* **Default**: unset\n\n```json\n{ \"model\": \"opus\" }\n```\n\n" +
	"### `permissions.allow`\n\nRules to allow.\n\n* **Scope**: `Any file`\n* **Type**: array of strings\n* **Default**: `[]`\n\n" +
	"### `newKey`\n\nOnly in a newer build.\n\n* **Scope**: `Any file`\n* **Type**: Boolean\n* **Default**: `false`\n\n" +
	"## Global config settings\n\n### `autoConnectIde`\n\nConnect to an IDE.\n\n* **Scope**: [`Global config`](#scopes)\n* **Type**: Boolean\n* **Default**: `false`\n"

const envDocMD = "## Variables\n\n| Variable | Purpose |\n| :- | :- |\n| `ANTHROPIC_API_KEY` | API key sent as `X-Api-Key` header |\n| `DOCUMENTED_ONLY` | Not read |\n"

// The reference goes with the newest build: what it documents, whether the build reads it, and
// what the build reads that it does not mention; a global config key is not counted unread.
func TestVendorDocs(t *testing.T) {
	saved := fetcher
	t.Cleanup(func() { fetcher = saved })
	fetcher = func(url string) ([]byte, error) {
		switch {
		case strings.HasSuffix(url, "settings-reference.md"):
			return []byte(settingsDocMD), nil
		case strings.HasSuffix(url, "env-vars.md"):
			return []byte(envDocMD), nil
		}
		return nil, os.ErrNotExist
	}
	docs := parseSettingsDoc(settingsDocMD)
	if m := docs["model"]; m == nil || m.Description != "The model to use. See models." || m.Scope != "Any file" || m.Type != "string" || m.Topic != "Model and responses" {
		t.Errorf("model = %+v", m)
	}
	s := &Snapshot{Harness: "claude", Version: "1.0.1", Config: map[string]any{
		"options":  map[string]any{"model": map[string]any{}, "permissions.allow": map[string]any{}, "quietHours": map[string]any{}, "$schema": map[string]any{}},
		"env_vars": []any{"ANTHROPIC_API_KEY", "CLAUDE_CODE_SECRET_FLAG"}}}
	d := docsOf(s, fetchDocs("claude", time.Now()))
	if got := strings.Join(d["undocumented_settings"].([]string), ","); got != "quietHours" {
		t.Errorf("undocumented settings = %s", got)
	}
	if got := strings.Join(d["documented_not_read"].([]string), ","); got != "newKey" {
		t.Errorf("documented, not read = %s", got)
	}
	if got := strings.Join(d["undocumented_env_vars"].([]string), ","); got != "CLAUDE_CODE_SECRET_FLAG" {
		t.Errorf("undocumented env = %s", got)
	}
	if got := strings.Join(d["documented_env_not_read"].([]string), ","); got != "DOCUMENTED_ONLY" {
		t.Errorf("documented env not read = %s", got)
	}
	if d["settings"].(map[string]any)["permissions.allow"].(map[string]any)["read"] != true {
		t.Errorf("a nested documented key the build reads read as unread")
	}
}

func TestDocScope(t *testing.T) {
	md := "## Remote\n\n### `remote.defaultEnvironmentId`\n\nThe environment.\n\n* **Scope**: [`Any file`](#scopes). For a self-hosted environment ID, user or managed settings only.\n* **Type**: string\n* **Default**: unset\n"
	d := parseSettingsDoc(md)["remote.defaultEnvironmentId"]
	if d == nil || d.Scope != "Any file" || d.ScopeNote != "For a self-hosted environment ID, user or managed settings only." {
		t.Errorf("scope = %+v", d)
	}
}

// A setting a minified reading missed on one build, though builds either side have it, is not
// observed there: no removal and no addition between them.
func TestUnobservedSettings(t *testing.T) {
	src := "the settings schema its bundle declares (zod, read from minified code)"
	snap := func(v string, keys ...string) *Snapshot {
		o := map[string]any{}
		for _, k := range keys {
			o[k] = map[string]any{"description": k}
		}
		return &Snapshot{Harness: "claude", Version: v, Config: map[string]any{"source": src, "options": o}}
	}
	by := map[string]*Snapshot{"1": snap("1", "model", "autoMode"), "2": snap("2", "model"), "3": snap("3", "model", "autoMode", "quietHours"), "4": snap("4", "model", "autoMode")}
	markUnobserved([]string{"1", "2", "3", "4"}, by)
	if got := by["2"].Config["unobserved"]; !reflect.DeepEqual(got, []string{"autoMode"}) {
		t.Errorf("unobserved in 2 = %v", got)
	}
	if by["4"].Config["unobserved"] != nil {
		t.Errorf("the newest build's missing setting is a removal, not unobserved: %v", by["4"].Config["unobserved"])
	}
	if d := refdiff.New(by["1"], by["2"], nil, time.Now()); len(d.Changes) != 0 {
		t.Errorf("a missed setting read as removed: %+v", d.Changes)
	}
	var ids []string
	for _, c := range refdiff.New(by["3"], by["4"], nil, time.Now()).Changes {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, " ") != "config.options.quietHours" {
		t.Errorf("3..4 = %v", ids)
	}
}

// A system message among the turns is prompt context; the tools it names as deferred are
// tools, and one that becomes deferred is one change.
func TestDeferredToolsAndSystemMessages(t *testing.T) {
	deferredText := "# Environment\nThe following deferred tools are available. Their schemas are NOT loaded, use ToolSearch:\nWebFetch\nWebSearch\n\nAgent types:\n- claude"
	p := claudeProfile("1.0.1", "Be brief.", []string{"Bash", "ToolSearch"}, "", "")
	var b map[string]any
	_ = json.Unmarshal(p.Requests[1].Body, &b)
	b["messages"] = append(b["messages"].([]any), map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": deferredText}}})
	p.Requests[1].Body, _ = json.Marshal(b)
	s := night{now: time.Now(), profiles: []e2e.Profile{p}}.tonight(buildID{"claude", "1.0.1"})
	var ctx []string
	for _, c := range s.Prompt["context"].([]any) {
		ctx = append(ctx, c.(map[string]any)["role"].(string))
	}
	if strings.Join(ctx, ",") != "user,system" {
		t.Errorf("context roles = %v", ctx)
	}
	var names []string
	for _, tl := range s.Tools {
		m := tl.(map[string]any)
		names = append(names, fmt.Sprintf("%v %v", m["name"], m["deferred"] == true))
	}
	if strings.Join(names, " ") != "Bash false ToolSearch false WebFetch true WebSearch true" {
		t.Errorf("tools = %v", names)
	}
	a := night{now: time.Now(), profiles: []e2e.Profile{claudeProfile("1.0.0", "Be brief.", []string{"Bash", "WebFetch"}, "", "")}}.tonight(buildID{"claude", "1.0.0"})
	d := refdiff.New(a, s, nil, time.Now())
	if c := changeByID(d, "tools.WebFetch.deferred"); c == nil || c.Title != "WebFetch is now a deferred tool (named only; its schema loaded on demand)" {
		t.Errorf("deferral = %+v", c)
	}
	for _, c := range d.Changes {
		if c.ID == "tools.WebFetch.description" || c.ID == "tools.WebFetch.input_schema" {
			t.Errorf("a deferral read as its schema removed: %s", c.ID)
		}
	}
	if c := changeByID(d, "tools.WebSearch"); c == nil || c.Title != "New deferred tool: WebSearch" {
		t.Errorf("new deferred = %+v", c)
	}
}

// A harness's own telemetry is read from its egress: Claude Code's events by their
// event_data's event_name, Clearcut's from the JSON its field holds; names and fields, no
// values; a run's count is no change.
func TestVendorTelemetry(t *testing.T) {
	claudeBatch := json.RawMessage(`{"events":[{"event_type":"ClaudeCodeInternalEvent","event_data":{"event_name":"tengu_feature_ok","device_id":"abc","env":{"platform":"darwin"}}},` +
		`{"event_type":"ClaudeCodeInternalEvent","event_data":{"event_name":"tengu_feature_ok","device_id":"abc"}},{"event_data":{"event_name":"tengu_run_hook","session_id":"s"}}]}`)
	clearcut := json.RawMessage(`[{"log_source_name":"CONCORD","log_event":[[{"source_extension_json":"{\"event_name\":\"startup_stats\",\"console_type\":\"GEMINI_CLI\"}"}]]}]`)
	tel := vendorTelemetry([]e2e.ProviderRequest{
		{Method: "POST", Host: "api.anthropic.com", Path: "/api/event_logging/v2/batch", Body: claudeBatch},
		{Method: "POST", Host: "play.googleapis.com", Path: "/log", Body: clearcut},
		{Method: "GET", Host: "api.anthropic.com", Path: "/mcp-registry/v0/servers"},
	})
	ep := tel["POST api.anthropic.com/api/event_logging/v2/batch"].(map[string]any)
	if got := strings.Join(sortedEventNames(ep), ","); got != "tengu_feature_ok,tengu_run_hook" {
		t.Errorf("events = %s", got)
	}
	ok := ep["events"].(map[string]any)["tengu_feature_ok"].(map[string]any)
	if ok["count"] != 2 || strings.Join(ok["fields"].([]string), ",") != "device_id,env,env.platform,event_name" {
		t.Errorf("tengu_feature_ok = %v", ok)
	}
	if strings.Contains(fmt.Sprint(tel), "abc") || strings.Contains(fmt.Sprint(tel), "darwin") {
		t.Error("a value reached the published telemetry")
	}
	if g := tel["POST play.googleapis.com/log"].(map[string]any); strings.Join(sortedEventNames(g), ",") != "startup_stats" {
		t.Errorf("clearcut = %v", g)
	}
	a := &Snapshot{Harness: "claude", Version: "1", Network: map[string]any{"telemetry": jsonOf(tel)}}
	tel2 := vendorTelemetry([]e2e.ProviderRequest{{Method: "POST", Host: "api.anthropic.com", Path: "/api/event_logging/v2/batch",
		Body: json.RawMessage(`{"events":[{"event_data":{"event_name":"tengu_feature_ok","device_id":"x","env":{"platform":"y"}}},{"event_data":{"event_name":"tengu_new","session_id":"s"}},{"event_data":{"event_name":"tengu_run_hook","session_id":"s"}}]}`)},
		{Method: "POST", Host: "play.googleapis.com", Path: "/log", Body: clearcut}})
	b := &Snapshot{Harness: "claude", Version: "2", Network: map[string]any{"telemetry": jsonOf(tel2)}}
	var titles []string
	for _, c := range refdiff.New(a, b, nil, time.Now()).Changes {
		titles = append(titles, c.Title)
	}
	if strings.Join(titles, " | ") != "New vendor telemetry event: tengu_new" {
		t.Errorf("changes = %v", titles)
	}
}

// A run's id after an underscore is the run's too: Claude Desktop's session ids.
func TestNormalizeUnderscoredUUID(t *testing.T) {
	for in, want := range map[string]string{
		"local_88d385d8-fbba-4414-a316-90943ceface5":                                "local_<uuid>",
		"<uuid>/4d2b7fd5-e813-4ae3-b573-50692bf90f1c":                               "<uuid>/<uuid>",
		"4d2b7fd5-e813-4ae3-b573-50692bf90f1c/88d385d8-fbba-4414-a316-90943ceface5": "<uuid>/<uuid>",
		"id=88D385D8-FBBA-4414-A316-90943CEFACE5_tmp":                               "id=<uuid>_tmp",
		"x88d385d8-fbba-4414-a316-90943ceface5":                                     "x88d385d8-fbba-4414-a316-90943ceface5",
	} {
		if got := normalizeText(in, nil, nil, nil); got != want {
			t.Errorf("normalizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Versions that rank alike (1.2 and 1.2.0) are listed in one order whatever order the published
// tree gave them, each diffed with its neighbour.
func TestVersionsSortWithTieBreak(t *testing.T) {
	for range 5 {
		dir := t.TempDir()
		var profiles []e2e.Profile
		for _, v := range []string{"1.2.0", "1.2", "1.1.9", "1.2.1"} {
			profiles = append(profiles, claudeProfile(v, "Be brief "+v+".", []string{"Bash"}, "", ""))
		}
		if _, err := publish(dir, night{now: time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC), profiles: profiles}, options{}); err != nil {
			t.Fatal(err)
		}
		var hi HarnessIndex
		if err := readJSON(filepath.Join(dir, "claude", "index.json"), &hi); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, v := range hi.Versions {
			got = append(got, v.Version)
		}
		if want := []string{"1.2.1", "1.2.0", "1.2", "1.1.9"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("versions = %q, want %q", got, want)
		}
		for _, f := range []string{"1.1.9..1.2.json", "1.2..1.2.0.json", "1.2.0..1.2.1.json"} {
			if _, err := os.Stat(filepath.Join(dir, "claude", "diffs", f)); err != nil {
				t.Errorf("diff %s: %v", f, err)
			}
		}
	}
}

// A harness id or version that could name a file outside the published tree is refused before
// anything is written: a vendor's feed is not trusted to stay inside it.
func TestPublishRefusesEscapingNames(t *testing.T) {
	for _, id := range []buildID{{"claude", "../../../escaped"}, {"../claude", "1.0.0"}, {"claude", "1.0..2"}, {"claude", ".hidden"}, {"claude", "1.0/2"}} {
		root := t.TempDir()
		dir := filepath.Join(root, "a", "b", "harnesses")
		p := claudeProfile(id.version, "Be brief.", []string{"Bash"}, "", "")
		p.Harness = id.harness
		_, err := publish(dir, night{now: time.Now(), profiles: []e2e.Profile{p}}, options{})
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%v: err = %v", id, err)
		}
		var written []string
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				written = append(written, path)
			}
			return nil
		})
		if len(written) > 0 {
			t.Errorf("%v: wrote %q", id, written)
		}
	}
	// A published snapshot whose version would escape is refused as it is loaded.
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "claude", "versions", "x.json"), &Snapshot{Harness: "claude", Version: "../../../escaped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := publish(dir, night{now: time.Now()}, options{}); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Errorf("loading an escaping version: %v", err)
	}
	for _, ok := range []string{"2.1.318", "0.2.1-rc.10", "26.1029.70936", "1.0.5+b", "1.0.0_beta"} {
		if err := checkBuild(buildID{"claude", ok}); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

// A changelog loaded back from what was published (changelog.json, an old diff's) is bounded
// as a fetched one is, before it is published again.
func TestLoadedChangelogIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i, v := range []string{"1.0.0", "1.0.1"} {
		n := night{now: time.Date(2026, 11, 3+i, 5, 0, 0, 0, time.UTC), profiles: []e2e.Profile{claudeProfile(v, "Be brief "+v+".", []string{"Bash"}, "", "")}}
		if _, err := publish(dir, n, options{}); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("x", refdiff.MaxChangelogLine*2)
	if err := writeJSON(filepath.Join(dir, "claude", "changelog.json"), Changelogs{SchemaVersion: 1, Harness: "claude",
		Versions: map[string]ChangelogNote{"0.9.0": {URL: "u", Text: long}}}); err != nil {
		t.Fatal(err)
	}
	diffPath := filepath.Join(dir, "claude", "diffs", "1.0.0..1.0.1.json")
	var d Diff
	if err := readJSON(diffPath, &d); err != nil {
		t.Fatal(err)
	}
	d.Changelog = &ChangelogNote{URL: "u", Text: long}
	if err := writeJSON(diffPath, &d); err != nil {
		t.Fatal(err)
	}
	if _, err := publish(dir, night{now: time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)}, options{}); err != nil {
		t.Fatal(err)
	}
	var cl Changelogs
	if err := readJSON(filepath.Join(dir, "claude", "changelog.json"), &cl); err != nil {
		t.Fatal(err)
	}
	for v, note := range cl.Versions {
		if len(note.Text) > refdiff.MaxChangelogLine || !note.Truncated {
			t.Errorf("changelog.json %s: %d bytes, truncated %v", v, len(note.Text), note.Truncated)
		}
	}
	if err := readJSON(diffPath, &d); err != nil {
		t.Fatal(err)
	}
	if d.Changelog == nil || len(d.Changelog.Text) > refdiff.MaxChangelogLine || !d.Changelog.Truncated {
		t.Errorf("the diff's changelog = %+v", d.Changelog)
	}
}

// An announcement's Slack message is the JSON it was when it was built of maps: what posts it
// reads the same fields.
func TestSlackMessageJSON(t *testing.T) {
	undoc, sig := 2, "revoked"
	for a, want := range map[*Announcement]string{
		{Harness: "claude", Name: "Claude Code", Version: "2.1.319", Previous: "2.1.318", URL: "https://terma.ai/h/claude/2.1.319",
			CompareURL: "https://terma.ai/harnesses/claude/compare/2.1.318...2.1.319", Changes: 4, Impact: ImpactN{"breaking": 1, "notable": 2, "degrading": 0},
			Undocumented: &undoc, Highlights: []string{"New tool: Monitor", "a <b> & c"}, Signing: &sig}: `{"blocks":[{"text":{"text":"*Claude Code 2.1.319* is out: 4 changes since 2.1.318\n1 breaking · 2 notable · 2 not in the vendor's changelog · code signature revoked","type":"mrkdwn"},"type":"section"},{"text":{"text":"• New tool: Monitor\n• a &lt;b&gt; &amp; c","type":"mrkdwn"},"type":"section"},{"elements":[{"text":{"text":"View changelog","type":"plain_text"},"type":"button","url":"https://terma.ai/h/claude/2.1.319"},{"text":{"text":"Compare with 2.1.318","type":"plain_text"},"type":"button","url":"https://terma.ai/harnesses/claude/compare/2.1.318...2.1.319"}],"type":"actions"}],"text":"Claude Code 2.1.319 is out: https://terma.ai/h/claude/2.1.319","unfurl_links":false}`,
		{Harness: "codex", Name: "Codex CLI", Version: "0.167.0", URL: "https://terma.ai/h/codex/0.167.0", Highlights: []string{}}: `{"blocks":[{"text":{"text":"*Codex CLI 0.167.0* is out","type":"mrkdwn"},"type":"section"},{"elements":[{"text":{"text":"View changelog","type":"plain_text"},"type":"button","url":"https://terma.ai/h/codex/0.167.0"}],"type":"actions"}],"text":"Codex CLI 0.167.0 is out: https://terma.ai/h/codex/0.167.0","unfurl_links":false}`,
	} {
		if got := string(marshalPlain(slackMessage(*a))); got != want {
			t.Errorf("slack message:\n got %s\nwant %s", got, want)
		}
	}
}

// Publishing reads a section's few fields through a typed view: what it reads is the same, and a
// field of another kind than it expects is left out without losing the rest.
func TestSectionViews(t *testing.T) {
	s := &Snapshot{Version: "1.0.0", Platform: "darwin/arm64",
		Support: &refdiff.Support{Status: "degraded", Capabilities: []refdiff.Capability{{ID: "a", Platform: "linux/amd64"}, {ID: "b"}}},
		Prompt: map[string]any{"system": []any{map[string]any{"text": "One."}, map[string]any{"text": "Two."}}, "context": "not a list",
			"interactive": map[string]any{"system": []any{map[string]any{"text": "Three."}}}},
		Binary: map[string]any{"signing": map[string]any{"status": "valid"}}}
	if got := platformsOf(s); !reflect.DeepEqual(got, []string{"darwin/arm64", "linux/amd64"}) {
		t.Errorf("platforms = %v", got)
	}
	if got := supportOf(s); got != "degraded" {
		t.Errorf("support = %q", got)
	}
	if got := supportOf(&Snapshot{}); got != "untested" {
		t.Errorf("no support = %q", got)
	}
	md := promptMarkdown("X", s)
	for _, want := range []string{"## System 1\n\nOne.\n", "## System 2\n\nTwo.\n", "## Interactive session: system\n\nThree.\n"} {
		if !strings.Contains(md, want) {
			t.Errorf("prompt markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "Injected context") {
		t.Errorf("a context of another kind is published:\n%s", md)
	}
	var bin binaryView
	view(s.Binary, &bin)
	if bin.Signing == nil || bin.Signing.Status.ptr() == nil || *bin.Signing.Status.ptr() != "valid" {
		t.Errorf("signing = %+v", bin.Signing)
	}
}

// timeline.json holds the newest steps; the older ones are in archives of the same size counted
// from the oldest; together they are every step, once.
func TestTimelineArchives(t *testing.T) {
	steps := func(n int) Timeline {
		tl := Timeline{SchemaVersion: 1, Harness: "codex"}
		for i := n; i >= 1; i-- { // newest first
			tl.Steps = append(tl.Steps, TimelineStep{Version: fmt.Sprintf("1.0.%d", i), Previous: fmt.Sprintf("1.0.%d", i-1), Changes: []TimelineChange{}})
		}
		return tl
	}
	read := func(dir, name string) Timeline {
		var tl Timeline
		if err := readJSON(filepath.Join(dir, name), &tl); err != nil {
			t.Fatal(err)
		}
		return tl
	}
	dir := t.TempDir()
	if err := writeTimeline(dir, steps(60), 25); err != nil {
		t.Fatal(err)
	}
	head := read(dir, "timeline.json")
	if len(head.Steps) != 25 || head.Steps[0].Version != "1.0.60" || head.Steps[24].Version != "1.0.36" {
		t.Errorf("timeline.json: %d steps, %s..%s", len(head.Steps), head.Steps[len(head.Steps)-1].Version, head.Steps[0].Version)
	}
	want := []TimelineArchive{{File: "timeline-1.json", First: "1.0.1", Last: "1.0.25", Steps: 25}, {File: "timeline-2.json", First: "1.0.26", Last: "1.0.35", Steps: 10}}
	if !reflect.DeepEqual(head.Archives, want) {
		t.Errorf("archives = %+v", head.Archives)
	}
	seen := map[string]bool{}
	for i, tl := range []Timeline{head, read(dir, "timeline-1.json"), read(dir, "timeline-2.json")} {
		if i > 0 && (tl.Archives != nil || tl.Harness != "codex" || tl.SchemaVersion != 1) {
			t.Errorf("archive %d = %+v", i, tl)
		}
		for i, s := range tl.Steps {
			if seen[s.Version] {
				t.Errorf("%s twice", s.Version)
			}
			seen[s.Version] = true
			if i > 0 && !versionLess(s.Version, tl.Steps[i-1].Version) {
				t.Errorf("not newest first at %s", s.Version)
			}
		}
	}
	if len(seen) != 60 {
		t.Errorf("%d steps of 60", len(seen))
	}
	full, _ := os.ReadFile(filepath.Join(dir, "timeline-1.json"))
	// A step more: the full archive stays as it was, the partial one takes the step.
	if err := writeTimeline(dir, steps(61), 25); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "timeline-1.json")); string(again) != string(full) {
		t.Error("a full archive changed")
	}
	if a := read(dir, "timeline.json").Archives; a[1].Steps != 11 || a[1].Last != "1.0.36" {
		t.Errorf("archives = %+v", a)
	}
	// Fewer steps than one file holds: no archives, and none left behind.
	if err := writeTimeline(dir, steps(20), 25); err != nil {
		t.Fatal(err)
	}
	if head := read(dir, "timeline.json"); len(head.Steps) != 20 || head.Archives != nil {
		t.Errorf("small timeline = %d steps, archives %+v", len(head.Steps), head.Archives)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "timeline-*.json")); len(left) != 0 {
		t.Errorf("archives left: %v", left)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "timeline.json"))
	if strings.Contains(string(data), "archives") {
		t.Errorf("a timeline with every step says archives:\n%s", data)
	}
}

// A night that measured less of a build's telemetry than the last keeps what it did not measure:
// the census alone keeps the probe's export and metric types, the probe alone every surface and
// its keys; what it did measure replaces the old.
func TestMergeKeepsTelemetryTonightDidNotMeasure(t *testing.T) {
	parse := func(doc string) map[string]any {
		var v map[string]any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	whole := `{"export":{"protocol":"http/protobuf"},"surfaces":{"logs/a":{"keys":{"k":{"class":"safe"}}},"metrics/m":{"keys":{},"type":"sum"}}}`
	snap := func(tel map[string]any) *Snapshot {
		return &Snapshot{Harness: "codex", Version: "1", Telemetry: tel, Capture: Capture{Command: []string{"codex"}}}
	}
	now := time.Now()

	censusOnly := parse(`{"surfaces":{"logs/a":{"keys":{"k":{"class":"safe"},"k2":{"class":"safe"}}},"metrics/m":{"keys":{}}}}`)
	got := merge(snap(parse(whole)), snap(censusOnly), now).Telemetry
	if got["export"] == nil || got["surfaces"].(map[string]any)["metrics/m"].(map[string]any)["type"] != "sum" {
		t.Errorf("a census-only night lost the probe's findings: %v", got)
	}
	if keys := got["surfaces"].(map[string]any)["logs/a"].(map[string]any)["keys"].(map[string]any); keys["k2"] == nil {
		t.Errorf("tonight's census did not replace the keys: %v", keys)
	}

	probeOnly := parse(`{"export":{"protocol":"http/json"},"surfaces":{"metrics/m":{"type":"gauge"}}}`)
	got = merge(snap(parse(whole)), snap(probeOnly), now).Telemetry
	surfaces := got["surfaces"].(map[string]any)
	if surfaces["logs/a"] == nil || surfaces["metrics/m"].(map[string]any)["keys"] == nil {
		t.Errorf("a probe-only night lost the census's surfaces or keys: %v", surfaces)
	}
	if got["export"].(map[string]any)["protocol"] != "http/json" || surfaces["metrics/m"].(map[string]any)["type"] != "gauge" {
		t.Errorf("tonight's probe did not replace the probe's findings: %v", got)
	}
}
