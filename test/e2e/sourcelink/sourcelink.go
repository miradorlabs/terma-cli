// Package sourcelink finds where a coding-agent harness's public source names something: a
// telemetry event or attribute, a tool, a header, an environment variable, a passage of its
// prompt. It reads a build's source from its release tag's tarball, for every harness whose
// source is public (Sources), and links the lines it finds on GitHub. compatgen links the
// drift digest's findings with it, and refgen the harness reference's changes.
//
// Tests, test modules and comments are not the build's source: a name a test keeps, or a
// comment still says, is nothing the build does. A string a constant holds is also named
// wherever the constant is, since the code that sends a surface often names it by one.
package sourcelink

import (
	"fmt"
	"slices"
	"strings"
)

// Ref is a line of a harness's source that names something.
type Ref struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	URL  string `json:"url"`
}

// Source is where a harness's source is public.
type Source struct {
	Repo string            // on GitHub, owner/name
	Tag  string            // a build's tag, with %s for its version
	Tags map[string]string // a build's tag where it is not Tag's
	Dirs []string          // the parts of the tree that hold the code that ships; none is all of it
	Lang string            // how its files are read: rust, typescript or python
}

// Sources are the harnesses whose source is public, by the id the e2e suite records them
// under; a build's version is the one it records. Claude Code and the GitHub Copilot CLI are
// closed: their repositories hold a changelog and plugins, not the harness.
var Sources = map[string]Source{
	"codex": {Repo: "openai/codex", Tag: "rust-v%s", Dirs: []string{"codex-rs/"}, Lang: "rust"},
	// The CLI and the core it runs; the IDE companion, the SDK and the A2A server ship apart.
	"gemini": {Repo: "google-gemini/gemini-cli", Tag: "v%s", Dirs: []string{"packages/cli/", "packages/core/"}, Lang: "typescript"},
	// opencode-ai is packages/opencode and the workspace packages it depends on; the web
	// console, the desktop app and the docs ship apart. Its vscode-v* tags are the extension's.
	"opencode": {Repo: "anomalyco/opencode", Tag: "v%s", Dirs: []string{
		"packages/opencode/", "packages/core/", "packages/llm/", "packages/server/", "packages/tui/",
		"packages/protocol/", "packages/codemode/", "packages/plugin/", "packages/schema/",
	}, Lang: "typescript"},
	// pi-coding-agent and the packages it depends on.
	"pi": {Repo: "earendil-works/pi", Tag: "v%s", Dirs: []string{
		"packages/coding-agent/", "packages/agent/", "packages/ai/", "packages/codemode/",
		"packages/mcp/", "packages/tui/", "packages/chord/",
	}, Lang: "typescript"},
	// The omp CLI and the packages it depends on; its Rust crates are natives it calls.
	"omp": {Repo: "can1357/oh-my-pi", Tag: "v%s", Dirs: []string{
		"packages/coding-agent/", "packages/agent/", "packages/ai/", "packages/catalog/", "packages/mnemopi/",
		"packages/natives/", "packages/tui/", "packages/utils/", "packages/wire/", "packages/snapcompact/",
		"packages/stats/", "packages/omptype/",
	}, Lang: "typescript"},
	// Every dsh release so far is a prerelease (0.2.1-rc.5), tagged as one.
	"dsh": {Repo: "deepseek-ai/deepseek-harness", Tag: "dsh-v%s", Dirs: []string{"apps/cli/", "packages/"}, Lang: "typescript"},
	// The t3 server (the npm package t3) and the providers it drives.
	"t3": {Repo: "pingdotgg/t3code", Tag: "v%s", Dirs: []string{"apps/server/", "packages/"}, Lang: "typescript"},
	// Hermes keeps its Python at the top of the tree. Its version is the one `hermes
	// --version` leads with; until 0.21.6 a build was tagged by its date instead.
	"hermes": {Repo: "NousResearch/hermes-agent", Tag: "v%s", Tags: hermesDateTags, Lang: "python"},
}

// hermesDateTags are the Hermes builds tagged by date, each by the version it reports
// (its release's name: "Hermes Agent v0.20.4 (2026.8.18)").
var hermesDateTags = map[string]string{
	"0.2.0": "v2026.3.12", "0.3.0": "v2026.3.17", "0.4.0": "v2026.3.23", "0.5.0": "v2026.3.28",
	"0.6.0": "v2026.3.30", "0.7.0": "v2026.4.3", "0.8.0": "v2026.4.8", "0.9.0": "v2026.4.13",
	"0.10.0": "v2026.4.16", "0.11.0": "v2026.4.23", "0.12.0": "v2026.4.30", "0.13.0": "v2026.5.7",
	"0.14.0": "v2026.5.16", "0.15.0": "v2026.5.28", "0.15.1": "v2026.5.29", "0.15.2": "v2026.5.29.2",
	"0.16.0": "v2026.6.5", "0.17.0": "v2026.6.19", "0.18.0": "v2026.7.1", "0.18.1": "v2026.7.7",
	"0.18.2": "v2026.7.7.2", "0.19.0": "v2026.7.20", "0.19.1": "v2026.7.30", "0.20.0": "v2026.8.3",
	"0.20.1": "v2026.8.13", "0.20.2": "v2026.8.16", "0.20.3": "v2026.8.16.2", "0.20.4": "v2026.8.18",
	"0.20.5": "v2026.8.19", "0.20.6": "v2026.8.27", "0.21.0": "v2026.8.31", "0.21.1": "v2026.9.7",
	"0.21.2": "v2026.9.11", "0.21.3": "v2026.9.14", "0.21.4": "v2026.9.21", "0.21.5": "v2026.9.24",
}

// Has reports whether harness's source is public.
func Has(harness string) bool {
	_, ok := Sources[harness]
	return ok
}

// Compare links the changes to harness's source from one build to another; none where its
// source is not public.
func Compare(harness, from, to string) string {
	s, ok := Sources[harness]
	if !ok {
		return ""
	}
	return s.Compare(from, to)
}

// URL is the repository's page.
func (s Source) URL() string { return "https://github.com/" + s.Repo }

// TagOf is the tag of a build.
func (s Source) TagOf(version string) string {
	if tag, ok := s.Tags[version]; ok {
		return tag
	}
	return fmt.Sprintf(s.Tag, version)
}

// Blob links a line of a build's source.
func (s Source) Blob(version, file string, line int) string {
	return fmt.Sprintf("https://github.com/%s/blob/%s/%s#L%d", s.Repo, s.TagOf(version), file, line)
}

// Compare links the changes from one build's source to another's.
func (s Source) Compare(from, to string) string {
	return fmt.Sprintf("https://github.com/%s/compare/%s...%s", s.Repo, s.TagOf(from), s.TagOf(to))
}

// unshipped are folders that hold no build's code wherever they sit: documentation (a docs
// site quoting the prompt), examples, the scripts that build and release it, benchmarks.
var unshipped = []string{"docs", "doc", "website", "i18n", "examples", "example", "scripts", "benchmarks"}

// holds reports whether file, a path from the top of the tree, is in the part that ships:
// under one of Dirs, in no unshipped folder, and no README, changelog or the instructions a
// repository keeps for the agents that work on it.
func (s Source) holds(file string) bool {
	if !(len(s.Dirs) == 0 || slices.ContainsFunc(s.Dirs, func(dir string) bool { return strings.HasPrefix(file, dir) })) {
		return false
	}
	dirs := strings.Split(file, "/")
	base := strings.ToUpper(dirs[len(dirs)-1])
	for _, dir := range dirs[:len(dirs)-1] {
		if slices.Contains(unshipped, dir) {
			return false
		}
	}
	for _, doc := range []string{"README", "CHANGELOG", "CONTRIBUTING", "LICENSE", "SECURITY", "AGENTS.MD", "CLAUDE.MD", "GEMINI.MD"} {
		if strings.HasPrefix(base, doc) {
			return false
		}
	}
	return true
}
