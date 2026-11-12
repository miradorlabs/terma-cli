package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// What the reference says of each harness besides what it observed: its name, vendor, where
// it is published and where its changelog is. Release dates come from the registry a harness
// is published to, and what its vendor says of a version from its changelog: a markdown file
// for a closed harness, the release's notes for one whose source is public.

// harnessMeta is one harness's identity.
type harnessMeta struct {
	name, vendor, homepage string
	npm                    string // the package it is published as, for release dates
	repo, language, tag    string // its public source, and a version's tag (%s)
	changelog              string // a page a person reads
	changelogRaw           string // a markdown changelog with a heading a version, where there is one
	feed                   string // a desktop app's: the harness whose update feed its release dates are read from
}

var harnesses = map[string]harnessMeta{
	"claude": {name: "Claude Code", vendor: "Anthropic", homepage: "https://claude.com/claude-code", npm: "@anthropic-ai/claude-code",
		changelog:    "https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md",
		changelogRaw: "https://raw.githubusercontent.com/anthropics/claude-code/main/CHANGELOG.md"},
	"claude-desktop": {name: "Claude Desktop (Code)", vendor: "Anthropic", homepage: "https://claude.ai/download", feed: "claude-desktop"},
	"codex": {name: "Codex CLI", vendor: "OpenAI", homepage: "https://github.com/openai/codex", npm: "@openai/codex",
		repo: "openai/codex", language: "rust", tag: "rust-v%s", changelog: "https://github.com/openai/codex/releases"},
	"codex-desktop": {name: "Codex app", vendor: "OpenAI", homepage: "https://openai.com/codex", feed: "codex-desktop"},
	"gemini": {name: "Gemini CLI", vendor: "Google", homepage: "https://github.com/google-gemini/gemini-cli", npm: "@google/gemini-cli",
		repo: "google-gemini/gemini-cli", language: "typescript", tag: "v%s", changelog: "https://github.com/google-gemini/gemini-cli/releases"},
	"opencode": {name: "OpenCode", vendor: "Anomaly", homepage: "https://opencode.ai", npm: "opencode-ai",
		repo: "anomalyco/opencode", language: "typescript", tag: "v%s", changelog: "https://github.com/anomalyco/opencode/releases"},
	"pi": {name: "Pi", vendor: "Earendil", homepage: "https://github.com/earendil-works/pi", npm: "@earendil-works/pi-coding-agent",
		repo: "earendil-works/pi", language: "typescript", tag: "v%s", changelog: "https://github.com/earendil-works/pi/releases"},
	"omp": {name: "oh-my-pi", vendor: "can1357", homepage: "https://github.com/can1357/oh-my-pi", npm: "@oh-my-pi/pi-coding-agent",
		repo: "can1357/oh-my-pi", language: "typescript", tag: "v%s", changelog: "https://github.com/can1357/oh-my-pi/releases"},
	"hermes": {name: "Hermes Agent", vendor: "Nous Research", homepage: "https://github.com/NousResearch/hermes-agent",
		repo: "NousResearch/hermes-agent", language: "python", tag: "v%s", changelog: "https://github.com/NousResearch/hermes-agent/releases"},
	"dsh": {name: "DeepSeek Harness", vendor: "DeepSeek", homepage: "https://github.com/deepseek-ai/deepseek-harness", npm: "@deepseek-ai/dsh",
		repo: "deepseek-ai/deepseek-harness", language: "typescript", tag: "dsh-v%s", changelog: "https://github.com/deepseek-ai/deepseek-harness/releases"},
	"copilot": {name: "GitHub Copilot CLI", vendor: "GitHub", homepage: "https://github.com/github/copilot-cli", npm: "@github/copilot",
		changelog:    "https://github.com/github/copilot-cli/blob/main/changelog.md",
		changelogRaw: "https://raw.githubusercontent.com/github/copilot-cli/main/changelog.md"},
	// Muse Code is closed, and published by Meta's download host alone: no registry dates its
	// builds, and no changelog is published.
	"muse": {name: "Muse Code", vendor: "Meta"},
	"t3": {name: "T3 Code", vendor: "Ping Labs", homepage: "https://github.com/pingdotgg/t3code", npm: "t3",
		repo: "pingdotgg/t3code", language: "typescript", tag: "v%s", changelog: "https://github.com/pingdotgg/t3code/releases"},
	"cursor": {name: "Cursor", vendor: "Anysphere", homepage: "https://cursor.com"},
}

func metaOf(id string) harnessMeta {
	if m, ok := harnesses[id]; ok {
		return m
	}
	return harnessMeta{name: id}
}

func (m harnessMeta) source() *SourceInfo {
	if m.repo == "" {
		return nil
	}
	return &SourceInfo{Repo: m.repo, URL: "https://github.com/" + m.repo, Language: m.language}
}

func (m harnessMeta) compare(from, to string) string {
	if m.repo == "" || m.tag == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/compare/%s...%s", m.repo, fmt.Sprintf(m.tag, from), fmt.Sprintf(m.tag, to))
}

// versionLess orders versions by their numbers, a prerelease before its release.
func versionLess(a, b string) bool { return refdiff.VersionLess(a, b) }

// fetcher reads the web for release dates and changelogs; tests replace it.
var fetcher = func(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" && strings.HasPrefix(url, "https://api.github.com/") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// releaseDates are when each version of a harness was published, from its registry.
func releaseDates(m harnessMeta) map[string]time.Time {
	if m.feed != "" {
		return feedDates(m.feed)
	}
	if m.npm == "" {
		return nil
	}
	data, err := fetcher("https://registry.npmjs.org/" + strings.ReplaceAll(m.npm, "/", "%2F"))
	if err != nil {
		return nil
	}
	var doc struct {
		Time map[string]time.Time `json:"time"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	return doc.Time
}

// feedDates are when each release a desktop app's update feed lists was published: Claude
// Desktop's lists its current release alone, so the others keep the date they had.
func feedDates(harness string) map[string]time.Time {
	data, err := fetcher(e2e.DesktopFeeds[harness])
	if err != nil {
		return nil
	}
	releases, err := e2e.ParseDesktopFeed(harness, data)
	if err != nil {
		return nil
	}
	out := map[string]time.Time{}
	for _, r := range releases {
		if !r.Released.IsZero() {
			out[r.Version] = r.Released
		}
	}
	return out
}

// changelogs reads what a harness's vendor says of its versions, once a harness a run.
type changelogs struct {
	markdown map[string]map[string]string // harness → version → section
}

var versionHeading = regexp.MustCompile(`(?m)^#{1,3}\s*\[?v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)\]?.*$`)

// sections splits a markdown changelog at its version headings.
func changelogSections(md string) map[string]string {
	out := map[string]string{}
	locs := versionHeading.FindAllStringSubmatchIndex(md, -1)
	for i, loc := range locs {
		end := len(md)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[md[loc[2]:loc[3]]] = strings.TrimSpace(md[loc[1]:end])
	}
	return out
}

// of is what the vendor says of a version, nil where it says nothing that can be read.
func (c *changelogs) of(harness, version string) *ChangelogNote {
	m := metaOf(harness)
	if m.changelogRaw != "" {
		if c.markdown == nil {
			c.markdown = map[string]map[string]string{}
		}
		secs, ok := c.markdown[harness]
		if !ok {
			if data, err := fetcher(m.changelogRaw); err == nil {
				secs = changelogSections(string(data))
			}
			c.markdown[harness] = secs
		}
		if text, ok := secs[version]; ok {
			return refdiff.BoundedNote(m.changelog, text)
		}
		return nil
	}
	if m.repo == "" || m.tag == "" {
		return nil
	}
	tag := fmt.Sprintf(m.tag, version)
	data, err := fetcher("https://api.github.com/repos/" + m.repo + "/releases/tags/" + tag)
	if err != nil {
		return nil
	}
	var rel struct {
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if json.Unmarshal(data, &rel) != nil || strings.TrimSpace(rel.Body) == "" {
		return nil
	}
	return refdiff.BoundedNote(rel.HTMLURL, strings.TrimSpace(rel.Body))
}
