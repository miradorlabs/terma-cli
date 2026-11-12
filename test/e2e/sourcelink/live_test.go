package sourcelink

import (
	"os"
	"strings"
	"testing"
)

// The real thing: TERMA_SOURCELINK_LIVE=1 reads a release of each harness from GitHub (into
// DefaultDir, or TERMA_SOURCELINK_CACHE) and finds names and a passage of its prompt that
// release is known to hold. Run with -v to see what it found. Normal runs read no network.

type liveCase struct {
	harness, version string
	needles          []string // each named somewhere in the build's code
	text             string   // a passage of a prompt the build holds
	textIn           string   // the file that holds it
}

var liveCases = []liveCase{
	{
		harness: "codex", version: "0.166.0",
		needles: []string{"codex.tool_decision", "codex.api_request", "apply_patch", "CODEX_HOME"},
		text: "You are Codex, based on GPT-5. You are running as a coding agent in the Codex CLI on a user's computer.\n\n" +
			"## General\n\n- When searching for text or files, prefer using `rg` or `rg --files` respectively because `rg` is " +
			"much faster than alternatives like `grep`. (If the `rg` command is not found, then use alternatives.)\n\n" +
			"## Editing constraints\n\n- Default to ASCII when editing or creating files. Only introduce non-ASCII or other " +
			"Unicode characters when there is a clear justification and the file already uses them.",
		textIn: "codex-rs/core/gpt_5_codex_prompt.md",
	},
	{
		harness: "gemini", version: "0.66.0",
		needles: []string{"gemini_cli.api_request", "gemini_cli.tool_call", "read_file", "run_shell_command", "GEMINI_API_KEY"},
		// As the build sends it: the template literal's escaped backticks read as backticks.
		text: "# Core Mandates\n\n## Security & System Integrity\n- **Credential Protection:** Never log, print, or commit " +
			"secrets, API keys, or sensitive credentials. Rigorously protect `.env` files, `.git`, and system configuration " +
			"folders.\n- **Source Control:** Do not stage or commit changes unless specifically requested by the user.",
		textIn: "packages/core/src/prompts/snippets.ts",
	},
	{
		harness: "opencode", version: "1.19.5",
		needles: []string{"webfetch", "OPENCODE_CONFIG_DIR", "bash"},
		text: "You are OpenCode, the best coding agent on the planet.\n\nYou are an interactive CLI tool that helps users " +
			"with software engineering tasks. Use the instructions below and the tools available to you to assist the user.\n\n" +
			"IMPORTANT: You must NEVER generate or guess URLs for the user unless you are confident that the URLs are for " +
			"helping the user with programming. You may use URLs provided by the user in their messages or local files.",
		textIn: "packages/opencode/src/session/prompt/anthropic.txt",
	},
	{
		harness: "hermes", version: "0.22.7",
		needles: []string{"HERMES_HOME", "terminal", "read_file"},
		// Concatenated Python strings, their escaped quotes read as quotes.
		text: "You are Hermes Agent, built by Nous Research. Be direct: match the length of your reply to the weight of " +
			"the ask — a one-line question gets a one-line answer, and finished work gets a short report of what changed, " +
			"what's verified, and what's left, never a replay of the process. No filler (\"Great question,\" \"I'd be happy " +
			"to\"), no restating the request back, no re-summarizing what you already said, no narrating tool calls the user can see.",
		textIn: "agent/prompt_builder.py",
	},
	// A build from before Hermes tagged by version, read at its date's tag.
	{harness: "hermes", version: "0.20.4", needles: []string{"HERMES_HOME"}},
	{
		harness: "pi", version: "1.5.0",
		needles: []string{"PI_OFFLINE", "bash"},
		text:    "You are a coding assistant running inside pi, a coding agent harness. Help users inspect files, run commands, and modify code.",
		textIn:  "packages/coding-agent/src/core/system-prompt.ts",
	},
	{
		harness: "omp", version: "18.16.0",
		needles: []string{"omp.tool.status"},
		text: "# Engineering\n- Correctness, then six-month maintainability. Delete dead weight; prefer boring design to " +
			"needless abstraction.\n- Compiled code: NEVER avoidable allocation, copying, computation.",
		textIn: "packages/coding-agent/src/prompts/system/system-prompt.md",
	},
	{
		harness: "dsh", version: "0.2.1-rc.5",
		needles: []string{"DEEPSEEK_BASE_URL"},
		text:    "You are an AI agent powered by DeepSeek Harness.",
		textIn:  "packages/core/system-prompt/src/index.ts",
	},
	{
		harness: "t3", version: "0.1.6",
		needles: []string{"T3CODE_DESKTOP_CLI_LAUNCHER", "codex"},
	},
}

func TestLiveSources(t *testing.T) {
	if os.Getenv("TERMA_SOURCELINK_LIVE") == "" {
		t.Skip("reads GitHub only with TERMA_SOURCELINK_LIVE=1")
	}
	dir := os.Getenv("TERMA_SOURCELINK_CACHE")
	if dir == "" {
		dir = DefaultDir()
	}
	l := New(dir)
	for _, c := range liveCases {
		t.Run(c.harness+"@"+c.version, func(t *testing.T) {
			s := Sources[c.harness]
			tag := "/blob/" + s.TagOf(c.version) + "/"
			lang, _ := languageOf(s.Lang)
			found, err := l.Find(c.harness, c.version, c.needles)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range c.needles {
				refs := found[n]
				if len(refs) == 0 {
					t.Errorf("%s %s: %q named nowhere", c.harness, c.version, n)
					continue
				}
				for _, r := range refs {
					if lang.testFile(r.Path) || !s.holds(r.Path) || !strings.Contains(r.URL, tag) {
						t.Errorf("%q: %+v is no shipping code of %s", n, r, s.TagOf(c.version))
					}
				}
				t.Logf("%q: %d lines, first %s", n, len(refs), refs[0].URL)
			}
			if c.text == "" {
				return
			}
			refs, err := l.FindText(c.harness, c.version, c.text)
			if err != nil {
				t.Fatal(err)
			}
			in := false
			for _, r := range refs {
				t.Logf("prompt passage: %s", r.URL)
				in = in || r.Path == c.textIn
			}
			// Codex keeps the passage word for word in each of three models' prompts, which
			// tie, so the file that holds it need not be first.
			if !in || len(refs) > 0 && !s.holds(refs[0].Path) {
				t.Errorf("the prompt passage is in %s: %+v", c.textIn, refs)
			}
		})
	}
}
