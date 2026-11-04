package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	a := &Snapshot{Harness: "codex", Version: "0.1.0", Support: map[string]any{"status": "supported"}}
	b := &Snapshot{Harness: "codex", Version: "0.1.1", Support: map[string]any{"status": "supported"}, Tools: []any{map[string]any{"name": "shell"}}}
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
	if a.Slack["blocks"] == nil || !strings.Contains(a.Slack["text"].(string), a.URL) {
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
		Support: map[string]any{"status": "supported", "capabilities": []any{
			map[string]any{"id": "relay.telemetry", "platform": "darwin/arm64", "result": "pass", "label": "Telemetry reaches the project"}}}}
	b := &Snapshot{Harness: "codex", Version: "0.1.1", Model: map[string]any{"default": "gpt-6.1-sol"},
		Support: map[string]any{"status": "supported", "capabilities": []any{
			map[string]any{"id": "relay.telemetry", "platform": "darwin/arm64", "result": "pass", "label": "Telemetry reaches the project"},
			map[string]any{"id": "global.commits", "platform": "linux/amd64", "result": "pass", "label": "Commits stamped"}}}}
	d := refdiff.New(a, b, &ChangelogNote{Text: "- Added GPT-6.1 Sol as the default model"}, time.Now())
	if len(d.Changes) != 1 || d.Changes[0].ID != "model.default" || !*d.Changes[0].Documented {
		t.Errorf("changes = %+v", d.Changes)
	}
	b.Support["capabilities"].([]any)[0].(map[string]any)["result"] = "fail"
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
