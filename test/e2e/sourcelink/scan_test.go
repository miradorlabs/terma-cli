package sourcelink

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// at renders refs as path:line, for comparing.
func at(refs []Ref) string {
	var out []string
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s:%d", r.Path, r.Line))
	}
	return strings.Join(out, " ")
}

// A TypeScript tree: a constant that names an event is followed to where it is used, an enum
// member by its enum's name; an import of the constant is no use of it; tests, test folders,
// declarations and comments name nothing; an environment variable is named by
// process.env; an object literal's key is a field.
func TestFindTypeScript(t *testing.T) {
	files := map[string]string{
		"packages/core/src/telemetry/constants.ts": "export const EVENT_API_REQUEST = 'gemini_cli.api_request';\n" +
			"export const EVENT_TOOL_CALL =\n  \"gemini_cli.tool_call\";\n",
		"packages/core/src/telemetry/loggers.ts": "import {\n  EVENT_API_REQUEST,\n  EVENT_TOOL_CALL,\n} from './constants.js';\n\n" +
			"export function logApiRequest(config: Config, event: ApiRequestEvent): void {\n" +
			"  const attributes = {\n    'event.name': EVENT_API_REQUEST,\n    model: event.model,\n  };\n  emit(attributes);\n}\n\n" +
			"export function logToolCall(event: ToolCallEvent): void {\n  emit({ 'event.name': EVENT_TOOL_CALL, function_name: event.name });\n}\n",
		"packages/core/src/config/models.ts": "export enum ApprovalMode {\n  DEFAULT = 'default',\n  YOLO = 'yolo',\n}\n\n" +
			"export function mode(m: string) {\n  return m === ApprovalMode.YOLO;\n}\n",
		"packages/core/src/env.ts":                    "export function key() {\n  return process.env.GEMINI_API_KEY ?? process.env['GOOGLE_API_KEY'];\n}\n",
		"packages/core/src/comments.ts":               "// emit('gemini_cli.in_comment')\n/* 'gemini_cli.in_block'\n */ emit(`gemini_cli.after_block`);\n",
		"packages/core/src/telemetry/loggers.test.ts": "expect(emit).toHaveBeenCalledWith('gemini_cli.api_request');\n",
		"packages/core/src/__tests__/x.ts":            "emit('gemini_cli.api_request');\n",
		"packages/core/src/types.d.ts":                "declare const X: 'gemini_cli.api_request';\n",
		"packages/vscode-ide-companion/src/a.ts":      "emit('gemini_cli.api_request');\n",
	}
	withSource(t, "fakets", Source{Repo: "acme/ts", Tag: "v%s", Dirs: []string{"packages/core/"}, Lang: "typescript"})
	l, _ := fakeLibrary(t, "", map[string][]byte{"acme/ts@v1.0.0": tarball(t, files)})
	got, err := l.Find("fakets", "1.0.0", []string{
		"gemini_cli.api_request", "gemini_cli.tool_call", "yolo", "GEMINI_API_KEY", "GOOGLE_API_KEY",
		"model", "gemini_cli.in_comment", "gemini_cli.in_block", "gemini_cli.after_block", "absent",
	})
	if err != nil {
		t.Fatal(err)
	}
	for needle, want := range map[string]string{
		// The constant's line, then its use, not its import.
		"gemini_cli.api_request": "packages/core/src/telemetry/constants.ts:1 packages/core/src/telemetry/loggers.ts:8",
		// A constant whose string is on the next line: the string's line, then the constant's.
		"gemini_cli.tool_call":   "packages/core/src/telemetry/constants.ts:3 packages/core/src/telemetry/constants.ts:2 packages/core/src/telemetry/loggers.ts:15",
		"yolo":                   "packages/core/src/config/models.ts:3 packages/core/src/config/models.ts:7",
		"GEMINI_API_KEY":         "packages/core/src/env.ts:2",
		"GOOGLE_API_KEY":         "packages/core/src/env.ts:2",
		"model":                  "packages/core/src/telemetry/loggers.ts:9",
		"gemini_cli.after_block": "packages/core/src/comments.ts:3",
	} {
		if s := at(got[needle]); s != want {
			t.Errorf("%s: %s, want %s", needle, s, want)
		}
	}
	for _, needle := range []string{"gemini_cli.in_comment", "gemini_cli.in_block", "absent"} {
		if _, ok := got[needle]; ok {
			t.Errorf("%s found: %+v", needle, got[needle])
		}
	}
	if u := got["gemini_cli.api_request"][1].URL; u != "https://github.com/acme/ts/blob/v1.0.0/packages/core/src/telemetry/loggers.ts#L8" {
		t.Errorf("url %s", u)
	}
	// Each use is in the function that sends the event.
	hits, err := l.Scan(Sources["fakets"], "1.0.0", map[Needle]bool{{Text: "gemini_cli.api_request"}: true, {Text: "gemini_cli.tool_call"}: true})
	if err != nil {
		t.Fatal(err)
	}
	if h := hits[Needle{Text: "gemini_cli.api_request"}][1]; h.Fn != 6 {
		t.Errorf("api_request is used in the function at line %d, want 6", h.Fn)
	}
	if h := hits[Needle{Text: "gemini_cli.tool_call"}][2]; h.Fn != 14 {
		t.Errorf("tool_call is used in the function at line %d, want 14", h.Fn)
	}
}

// What starts a TypeScript function: declarations, arrows, methods and classes; not a
// control statement or a call.
func TestTypeScriptFunctionStarts(t *testing.T) {
	for line, want := range map[string]bool{
		"export async function logApiRequest(config: Config): Promise<void> {": true,
		"function f() {": true,
		"export const handler = async (req: Request) => {":             true,
		"const f = (a, b) => a + b;":                                   true,
		"  toOpenTelemetryAttributes(config: Config): LogAttributes {": true,
		"  async run(): Promise<void> {":                               true,
		"  private async execute(":                                     true,
		"  ): Promise<ToolResult> {":                                   true,
		"  handle = async (event: Event) => {":                         true,
		"export class ApiRequestEvent implements BaseTelemetryEvent {": true,
		"  if (enabled) {":                                             false,
		"  } else if (x) {":                                            false,
		"  for (const x of xs) {":                                      false,
		"  logger.emit(record);":                                       false,
		"  doSomething(":                                               false,
		"  const total = (a + b) * 2;":                                 false,
		"  ) {":                                                        false,
	} {
		if got := tsFunction(line); got != want {
			t.Errorf("%q starts a function: %v, want %v", line, got, want)
		}
	}
}

// A Python tree: a module's constant is followed to where it is used, not a class's; tests,
// conftest and test folders name nothing; comments and docstrings name nothing; a keyword
// argument is a field; def starts a function.
func TestFindPython(t *testing.T) {
	files := map[string]string{
		"agent/telemetry.py": "\"\"\"Telemetry: emits \"hermes.in_docstring\".\"\"\"\nfrom agent.names import (\n    TOOL_EVENT,\n)\n\n" +
			"TURN_EVENT = \"hermes.turn\"\n\nclass Events:\n    LOCAL = \"hermes.class_constant\"\n\n" +
			"def record_turn(model, tokens):\n    # emit(\"hermes.in_comment\")\n    emit(TURN_EVENT, model=model, tokens=tokens)\n\n" +
			"async def record_tool(name):\n    emit(TOOL_EVENT, tool_name=name)\n    emit(Events.LOCAL)\n",
		"agent/names.py":          "TOOL_EVENT = 'hermes.tool'\n",
		"tests/test_telemetry.py": "emit('hermes.turn')\n",
		"agent/conftest.py":       "emit('hermes.turn')\n",
		"agent/telemetry_test.py": "emit('hermes.turn')\n",
		"agent/env.py":            "import os\nKEY = os.environ.get(\"HERMES_API_KEY\")\n",
	}
	withSource(t, "fakepy", Source{Repo: "acme/py", Tag: "v%s", Lang: "python"})
	l, _ := fakeLibrary(t, "", map[string][]byte{"acme/py@v1.0.0": tarball(t, files)})
	got, err := l.Find("fakepy", "1.0.0", []string{"hermes.turn", "hermes.tool", "hermes.class_constant", "HERMES_API_KEY", "tool_name", "hermes.in_comment", "hermes.in_docstring"})
	if err != nil {
		t.Fatal(err)
	}
	for needle, want := range map[string]string{
		"hermes.turn":           "agent/telemetry.py:6 agent/telemetry.py:13",
		"hermes.tool":           "agent/names.py:1 agent/telemetry.py:16",
		"hermes.class_constant": "agent/telemetry.py:9",
		"HERMES_API_KEY":        "agent/env.py:2",
		"tool_name":             "agent/telemetry.py:16",
	} {
		if s := at(got[needle]); s != want {
			t.Errorf("%s: %s, want %s", needle, s, want)
		}
	}
	for _, needle := range []string{"hermes.in_comment", "hermes.in_docstring"} {
		if _, ok := got[needle]; ok {
			t.Errorf("%s found: %+v", needle, got[needle])
		}
	}
	hits, err := l.Scan(Sources["fakepy"], "1.0.0", map[Needle]bool{{Text: "hermes.tool"}: true})
	if err != nil {
		t.Fatal(err)
	}
	if h := hits[Needle{Text: "hermes.tool"}][1]; h.Fn != 15 {
		t.Errorf("hermes.tool is used in the function at line %d, want 15", h.Fn)
	}
}

// Rust is read as compatgen reads Codex: its tests and comments name nothing, a constant is
// followed past its import, and a tracing macro's field is a field.
func TestFindRust(t *testing.T) {
	files := map[string]string{
		"codex-rs/otel/src/names.rs": "pub const TOOL_CALL_METRIC: &str = \"codex.tool.call\";\n",
		"codex-rs/otel/src/emit.rs":  "use crate::names::TOOL_CALL_METRIC;\n\nfn emit() {\n    // \"codex.tool.call\"\n    counter(TOOL_CALL_METRIC, 1);\n    info_span!(\"turn\", codex.turn.phase = \"x\");\n}\n",
		"codex-rs/core/tests/a.rs":   "fn t() { counter(\"codex.tool.call\"); }\n",
		"codex-cli/bin/codex.js":     "emit('codex.tool.call');\n",
	}
	withSource(t, "fakers", Source{Repo: "acme/rs", Tag: "rust-v%s", Dirs: []string{"codex-rs/"}, Lang: "rust"})
	l, _ := fakeLibrary(t, "", map[string][]byte{"acme/rs@rust-v0.1.0": tarball(t, files)})
	got, err := l.Find("fakers", "0.1.0", []string{"codex.tool.call", "codex.turn.phase"})
	if err != nil {
		t.Fatal(err)
	}
	if s := at(got["codex.tool.call"]); s != "codex-rs/otel/src/names.rs:1 codex-rs/otel/src/emit.rs:5" {
		t.Errorf("codex.tool.call: %s", s)
	}
	if s := at(got["codex.turn.phase"]); s != "codex-rs/otel/src/emit.rs:6" {
		t.Errorf("codex.turn.phase: %s", s)
	}
}

// A build is read once a process however many lookups ask about it, and once a cache
// directory: a second library on the same directory reads nothing from the network. A build
// that cannot be read says why, once.
func TestLibraryReadsEachBuildOnce(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"src/a.ts": "emit('one');\nemit('two');\n", "src/prompt.md": "You are a helpful agent that writes careful code.\n"}
	builds := map[string][]byte{"acme/ts@v1.0.0": tarball(t, files)}
	withSource(t, "fakets", Source{Repo: "acme/ts", Tag: "v%s", Lang: "typescript"})
	l, opened := fakeLibrary(t, dir, builds)
	for _, n := range []string{"one", "two"} {
		if got, err := l.Find("fakets", "1.0.0", []string{n}); err != nil || len(got[n]) != 1 {
			t.Fatalf("%s: %v %v", n, got, err)
		}
	}
	if refs, err := l.FindText("fakets", "1.0.0", "You are a helpful agent that writes careful code."); err != nil || len(refs) != 1 {
		t.Fatalf("text: %v %v", refs, err)
	}
	if opened["acme/ts@v1.0.0"] != 1 {
		t.Errorf("read %d times", opened["acme/ts@v1.0.0"])
	}
	if _, err := os.Stat(filepath.Join(dir, "acme_ts", "v1.0.0.tar.gz")); err != nil {
		t.Errorf("not cached: %v", err)
	}
	again, openedAgain := fakeLibrary(t, dir, builds)
	if got, err := again.Find("fakets", "1.0.0", []string{"two"}); err != nil || len(got["two"]) != 1 || openedAgain["acme/ts@v1.0.0"] != 0 {
		t.Errorf("from the cache: %v %v, read %d times", got, err, openedAgain["acme/ts@v1.0.0"])
	}
	for range 2 {
		if _, err := l.Find("fakets", "9.9.9", []string{"one"}); err == nil || err.Error() != "404 Not Found" {
			t.Errorf("a missing build: %v", err)
		}
	}
	if opened["acme/ts@v9.9.9"] != 1 {
		t.Errorf("a missing build was asked for %d times", opened["acme/ts@v9.9.9"])
	}
	if _, err := l.Find("claude", "2.1.0", []string{"x"}); !errors.Is(err, ErrNotPublic) {
		t.Errorf("a closed harness: %v", err)
	}
}

// A library with no directory keeps its tarballs only until Close.
func TestLibraryWithoutADirectoryCleansUp(t *testing.T) {
	withSource(t, "fakets", Source{Repo: "acme/ts", Tag: "v%s", Lang: "typescript"})
	l, _ := fakeLibrary(t, "", map[string][]byte{"acme/ts@v1.0.0": tarball(t, map[string]string{"a.ts": "emit('x');\n"})})
	if _, err := l.Find("fakets", "1.0.0", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	tmp := l.tmp
	if tmp == "" {
		t.Fatal("no temporary directory")
	}
	l.Close()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("%s left behind: %v", tmp, err)
	}
}

// Every harness whose source is public has a tag for its version, a language it is read in,
// and links on GitHub; Hermes's builds before 0.21.6 are tagged by date.
func TestSources(t *testing.T) {
	for _, h := range []string{"codex", "gemini", "opencode", "pi", "omp", "dsh", "t3", "hermes"} {
		s, ok := Sources[h]
		if !ok {
			t.Errorf("%s has no source", h)
			continue
		}
		if _, err := languageOf(s.Lang); err != nil || !strings.Contains(s.Tag, "%s") {
			t.Errorf("%s: %+v %v", h, s, err)
		}
	}
	for _, h := range []string{"claude", "copilot", "claude-desktop", "codex-desktop"} {
		if Has(h) {
			t.Errorf("%s is closed source", h)
		}
	}
	for version, tag := range map[string]string{"0.20.4": "v2026.8.18", "0.21.5": "v2026.9.24", "0.21.6": "v0.21.6", "0.22.7": "v0.22.7"} {
		if got := Sources["hermes"].TagOf(version); got != tag {
			t.Errorf("hermes %s is tagged %s, want %s", version, got, tag)
		}
	}
	if got := Compare("hermes", "0.21.5", "0.21.6"); got != "https://github.com/NousResearch/hermes-agent/compare/v2026.9.24...v0.21.6" {
		t.Errorf("compare %s", got)
	}
	if got := Compare("dsh", "0.2.1-rc.4", "0.2.1-rc.5"); got != "https://github.com/deepseek-ai/deepseek-harness/compare/dsh-v0.2.1-rc.4...dsh-v0.2.1-rc.5" {
		t.Errorf("compare %s", got)
	}
	if Compare("claude", "1", "2") != "" {
		t.Error("a closed harness compared")
	}
	if !slices.Contains(Sources["opencode"].Dirs, "packages/opencode/") || Sources["opencode"].holds("packages/console/app.ts") {
		t.Errorf("opencode reads %v", Sources["opencode"].Dirs)
	}
	// Documentation is not the build, wherever it sits.
	for file, want := range map[string]bool{
		"agent/prompt_builder.py":                  true,
		"website/docs/developer-guide/prompts.md":  false,
		"packages/core/docs/telemetry.md":          false,
		"agent/README.md":                          false,
		"AGENTS.md":                                false,
		"codex-rs/core/gpt_5_codex_prompt.md":      true,
		"codex-rs/core/templates/agents/review.md": true,
	} {
		if got := (Source{}).holds(file); got != want {
			t.Errorf("%s ships: %v, want %v", file, got, want)
		}
	}
}
