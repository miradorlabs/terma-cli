package main

import (
	"context"
	"reflect"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 10, 6, 45, 0, 0, time.UTC)

func byID(obs []observation) map[string]observation {
	m := map[string]observation{}
	for _, o := range obs {
		m[o.id] = o
	}
	return m
}

func wantFacts(t *testing.T, id string, f map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if !reflect.DeepEqual(f[k], v) {
			t.Errorf("%s: %s = %#v, want %#v", id, k, f[k], v)
		}
	}
}

func TestAnthropicDocs(t *testing.T) {
	const base = "https://platform.claude.com/docs/en"
	c, _ := newFakeWeb(t, map[string]string{
		base + "/about-claude/model-deprecations.md":      testdata(t, "anthropic/model-deprecations.md"),
		base + "/about-claude/models/overview.md":         testdata(t, "anthropic/overview.md"),
		base + "/models/fable-5-2/overview.md":            testdata(t, "anthropic/fable-5-2.md"),
		base + "/models/fable-5-2/whats-new-fable-5-2.md": testdata(t, "anthropic/whats-new-fable-5-2.md"),
	})
	obs, err := anthropicDocs(context.Background(), c, testNow)
	if err != nil {
		t.Fatal(err)
	}
	m := byID(obs)
	fable, ok := m["claude-fable-5-2"]
	if !ok || !fable.listed {
		t.Fatalf("claude-fable-5-2 not listed: %v", m)
	}
	wantFacts(t, "claude-fable-5-2", fable.facts, map[string]any{
		factName: "Claude Fable 5.2", factStatus: "active", factRetiresNotBefore: "2027-11-09", factReleasedAt: "2026-11-09",
		factContextWindow: int64(1_000_000), factMaxOutput: int64(128_000), factDefaultEffort: "high",
		factKnowledgeCutoff: "2026-06", factThinking: "adaptive (always on)", factReasoning: true, factLatency: "moderate",
		factAliases: []string{"claude-fable-5-2"},
	})
	if ids, _ := fable.facts[factPlatformIDs].(map[string]string); ids["Amazon Bedrock"] != "anthropic.claude-fable-5-2" {
		t.Errorf("platform ids = %v", fable.facts[factPlatformIDs])
	}
	if fable.docs["overview"].URL != base+"/models/fable-5-2/overview" || fable.docs["whats_new"].Text == "" {
		t.Errorf("docs = %v", fable.docs)
	}
	// A model whose page there is none of is still followed, from the status table.
	sonnet := m["claude-sonnet-4-5-20250929"]
	wantFacts(t, "claude-sonnet-4-5-20250929", sonnet.facts, map[string]any{
		factStatus: "deprecated", factDeprecatedAt: "2026-09-30", factRetiresAt: "2026-11-30"})
	if _, ok := sonnet.facts[factRetiresNotBefore]; ok {
		t.Error("a retirement date is not a no-sooner-than")
	}
}

func TestAnthropicDocsWithoutStatusTableFails(t *testing.T) {
	// A page that lost its table must not read as "Anthropic lists no models".
	c, _ := newFakeWeb(t, map[string]string{
		"https://platform.claude.com/docs/en/about-claude/model-deprecations.md": "# Model deprecations\n\nNothing here.\n",
	})
	if _, err := anthropicDocs(context.Background(), c, testNow); err == nil {
		t.Fatal("no error for a status page without its table")
	}
}

func TestOpenAIDocs(t *testing.T) {
	const base = "https://developers.openai.com/api/docs"
	c, _ := newFakeWeb(t, map[string]string{
		base + "/models.md": "# Models\n\n- [GPT-6.1 Sol](/api/docs/models/gpt-6.1-sol.md): Near-Astra.\n" +
			"- [GPT-Audio-1.5](/api/docs/models/gpt-audio-1.5.md): Voice.\n- [all models](/api/docs/models/all.md)\n",
		base + "/models/gpt-6.1-sol.md": testdata(t, "openai/gpt-6.1-sol.md"),
		base + "/models/gpt-audio-1.5.md": "# GPT-Audio-1.5\n\nModel ID: `gpt-audio-1.5`\n\n## Model details\n\n- Input modalities: text, audio\n" +
			"- Output modalities: text, audio\n\n## Endpoints\n\n| Endpoint | Route | Support |\n| --- | --- | --- |\n| Chat Completions | `v1/chat/completions` | Supported |\n",
	})
	obs, err := openAIDocs(context.Background(), c, testNow)
	if err != nil {
		t.Fatal(err)
	}
	m := byID(obs)
	if _, ok := m["gpt-audio-1.5"]; ok {
		t.Error("a voice model is followed")
	}
	sol, ok := m["gpt-6.1-sol"]
	if !ok {
		t.Fatalf("gpt-6.1-sol not followed: %v", m)
	}
	// The effort levels' sentence wraps onto a second line in the page: every level is read,
	// and nothing after the sentence's end (regression: `reasoning_effort` read as a level).
	wantFacts(t, "gpt-6.1-sol", sol.facts, map[string]any{
		factName: "GPT-6.1 Sol", factContextWindow: int64(1_050_000), factMaxInput: int64(922_000), factMaxOutput: int64(128_000),
		factKnowledgeCutoff: "2026-04-30", factReasoning: true, factDefaultEffort: "medium",
		factReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"},
		factInputModalities:  []string{"text", "image"}, factOutputModalities: []string{"text"},
		factEndpoints: []string{"v1/chat/completions", "v1/responses", "v1/batch"}, factToolCalling: true, factStructuredOutputs: true,
	})
	if ids, _ := sol.facts[factPlatformIDs].(map[string]string); ids["Amazon Bedrock"] != "openai.gpt-6.1-sol" || ids["Google Cloud"] != "gpt-6.1-sol" {
		t.Errorf("platform ids = %v", sol.facts[factPlatformIDs])
	}
}

func TestGoogleDocs(t *testing.T) {
	const base = "https://ai.google.dev/gemini-api/docs"
	c, _ := newFakeWeb(t, map[string]string{
		base + "/models.md.txt":                   `# Models` + "\n\n" + `{% card href="/docs/models/gemini-4-argon" /%}` + "\n",
		base + "/models/gemini-4-argon.md.txt":    testdata(t, "google/gemini-4-argon.md.txt"),
		base + "/whats-new-gemini-4-argon.md.txt": testdata(t, "google/whats-new-gemini-4-argon.md.txt"),
	})
	obs, err := googleDocs(context.Background(), c, testNow)
	if err != nil {
		t.Fatal(err)
	}
	argon, ok := byID(obs)["gemini-4-argon"]
	if !ok {
		t.Fatalf("gemini-4-argon not followed: %v", obs)
	}
	wantFacts(t, "gemini-4-argon", argon.facts, map[string]any{
		factName: "Gemini 4 Argon", factContextWindow: int64(1_048_576), factMaxOutput: int64(65_536),
		factInputModalities: []string{"text", "image", "video", "audio", "pdf"}, factOutputModalities: []string{"text"},
		factReasoning: true, factToolCalling: true, factStructuredOutputs: true,
	})
	if _, ok := argon.facts[factReleasedAt]; ok {
		t.Error(`a page's "latest update" is no release date`)
	}
	if argon.docs["whats_new"].Text == "" {
		t.Error("no what's new page")
	}
}

func TestXAIDocs(t *testing.T) {
	const base = "https://docs.x.ai/developers"
	c, _ := newFakeWeb(t, map[string]string{
		base + "/models.md": "# Models\n\n### Text API Pricing\n\n| Model | Context |\n| --- | --- |\n" +
			"| grok-4.7 (< 200k prompt tokens) | 500k |\n| grok-4.7 (≥ 200k prompt tokens) | 500k |\n| grok-build-0.1 (< 200k prompt tokens) | 256k |\n\n### Imagine Pricing\n",
		base + "/models/grok-4.7.md": testdata(t, "xai/grok-4.7.md"),
	})
	obs, err := xaiDocs(context.Background(), c, testNow)
	if err != nil {
		t.Fatal(err)
	}
	m := byID(obs)
	if len(m) != 2 {
		t.Fatalf("followed %d models, want grok-4.7 and grok-build-0.1 (one without a page): %v", len(m), m)
	}
	wantFacts(t, "grok-4.7", m["grok-4.7"].facts, map[string]any{
		factName: "Grok 4.7", factContextWindow: int64(500_000), factBatch: true, factToolCalling: true, factReasoning: true,
		factReasoningEfforts: []string{"low", "medium", "high", "xhigh"}, factDefaultEffort: "high",
		factInputModalities: []string{"text", "image"}, factOutputModalities: []string{"text"},
		factRegions: []string{"us-east-1", "us-west-2", "us-central-1"},
	})
}

func TestChangelogs(t *testing.T) {
	for _, tc := range []struct {
		v           *vendor
		file        string
		first, last string
		id, name    string
		mentions    int
	}{
		{anthropic, "anthropic/release-notes.md", "2026-11-09", "2026-10-30", "claude-fable-5-2", "Claude Fable 5.2", 7}, // the eighth says "Both models"
		{openAI, "openai/changelog.md", "2026-11-05", "2026-10-30", "gpt-6.1-sol", "GPT-6.1 Sol", -1},
		{google, "google/changelog.md.txt", "2026-11-06", "", "gemini-4-argon", "Gemini 4 Argon", -1},
		// xAI's months name no year: November is this year's, September too.
		{xAI, "xai/release-notes.md", "2026-11", "2026-09", "grok-4.7", "Grok 4.7", 1},
	} {
		entries := splitChangelog(testdata(t, tc.file), tc.v, testNow)
		if len(entries) == 0 {
			t.Errorf("%s: no entries", tc.v.id)
			continue
		}
		if entries[0].Date != tc.first {
			t.Errorf("%s: first date %s, want %s", tc.v.id, entries[0].Date, tc.first)
		}
		if tc.last != "" && entries[len(entries)-1].Date != tc.last {
			t.Errorf("%s: last date %s, want %s", tc.v.id, entries[len(entries)-1].Date, tc.last)
		}
		if n := len(mentions(entries, tc.id, tc.name)); tc.mentions >= 0 && n != tc.mentions {
			t.Errorf("%s: %d entries name %s, want %d", tc.v.id, n, tc.id, tc.mentions)
		}
	}
}

func TestXAIYearsRollBack(t *testing.T) {
	text := "## February\n\n### A\n\nOne.\n\n## January\n\n### B\n\nTwo.\n\n## December\n\n### C\n\nThree.\n"
	got := splitChangelog(text, xAI, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	var dates []string
	for _, e := range got {
		dates = append(dates, e.Date)
	}
	want := []string{"2026-02", "2026-01", "2025-12"}
	if !reflect.DeepEqual(dates, want) {
		t.Errorf("dates = %v, want %v", dates, want)
	}
}

func TestNamesAreWhole(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"We launched Claude Fable 5 today.", true},
		{"We launched Claude Fable 5.1 today.", false},
		{"Claude Fable 5, Claude Opus 5", true},
		{"use `claude-fable-5-1`", false},
		{"use `claude-fable-5`.", true},
	} {
		if got := names(tc.text, "claude-fable-5", "Claude Fable 5"); got != tc.want {
			t.Errorf("names(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestVendorHighlights(t *testing.T) {
	ant := Page{Text: testdata(t, "anthropic/whats-new-fable-5-2.md")}
	got := vendorHighlights(&ant, nil, 5)
	if len(got) != 4 || got[0] != "Editing earlier turns invalidates thinking blocks on every account" {
		t.Errorf("Anthropic: %q", got)
	}
	gem := Page{Text: testdata(t, "google/whats-new-gemini-4-argon.md.txt")}
	got = vendorHighlights(&gem, nil, 5)
	if len(got) == 0 || got[0] != "Unified 1M input-output token limit" {
		t.Errorf("Google: %q", got)
	}
	// OpenAI's entries open with a line of tags, xAI's with a heading: neither is the news.
	got = vendorHighlights(nil, []ChangelogEntry{
		{Text: "Feature · Model: gpt-6.1-sol · API: v1/responses\n\nReleased [GPT-6.1 Sol](/x) for complex work. More text."},
		{Text: "### Grok 4.7\n\nGrok 4.7 is now available on the xAI API. It has a 500k context."},
	}, 5)
	want := []string{"Released GPT-6.1 Sol for complex work.", "Grok 4.7 is now available on the xAI API."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changelog highlights = %q, want %q", got, want)
	}
}

func TestParsers(t *testing.T) {
	for in, want := range map[string]int64{"1M": 1_000_000, "128K tokens": 128_000, "500k": 500_000, "1,050,000 context window": 1_050_000, "65,536": 65_536} {
		if got, ok := tokenCount(in); !ok || got != want {
			t.Errorf("tokenCount(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for in, want := range map[string]string{"November 9, 2026": "2026-11-09", "Nov 5, 2026": "2026-11-05", "Jun 2026": "2026-06",
		"Apr 30, 2026 knowledge cutoff": "2026-04-30", "2026-06": "2026-06", "Not sooner than November 9, 2027": "2027-11-09", "N/A": ""} {
		if got := isoDate(in); got != want {
			t.Errorf("isoDate(%q) = %q, want %q", in, got, want)
		}
	}
}
