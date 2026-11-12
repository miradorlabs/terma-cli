package sourcelink

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// language is how a source's files are read: which are its code, which its tests, where a
// function starts, what a constant naming a string looks like, and what names a field.
type language struct {
	exts     []string // its code's files
	skip     []string // files with one of exts that are no code: declarations, minified bundles
	testFile func(file string) bool
	// fnStart reports whether a line starts a function, which places a key beside its
	// surface.
	fnStart func(line string) bool
	// constName is a constant that names a string, its name and string the first two
	// groups; constHead one whose string is on the next line.
	constName, constHead *regexp.Regexp
	// constant are the names a line may use a constant by.
	constant []*regexp.Regexp
	// imported reports whether a line (trimmed) imports names rather than using them,
	// following an import across lines.
	imported func(t string, in *bool) bool
	// fields are the names a line assigns, the way a tracing macro names a field
	// ("codex.turn.phase = …") or an object literal its key ("model: m").
	fields func(line string) []string
	// quoted are names a line uses as it would a string ("process.env.X").
	quoted func(line string) []string
	// enums reads TypeScript enums: a member that names a string is a constant,
	// Enum.Member.
	enums bool
	// testModules skips Rust's test modules (#[cfg(test)] mod tests { … }).
	testModules bool
}

// proseExts are the files FindText reads whole: prompts and descriptions kept as text.
var proseExts = []string{".md", ".mdx", ".markdown", ".txt", ".prompt", ".j2", ".jinja", ".jinja2", ".tmpl", ".hbs", ".mustache", ".yaml", ".yml", ".toml"}

func languageOf(name string) (*language, error) {
	switch name {
	case "rust":
		return rust, nil
	case "typescript", "javascript":
		return typescript, nil
	case "python":
		return python, nil
	}
	return nil, fmt.Errorf("no rules for reading %q source", name)
}

// code reports whether file is the language's code.
func (l *language) code(file string) bool {
	return slices.ContainsFunc(l.exts, func(ext string) bool { return strings.HasSuffix(file, ext) }) &&
		!slices.ContainsFunc(l.skip, func(ext string) bool { return strings.HasSuffix(file, ext) })
}

// upperName is a name a constant goes by in every language here: SCREAMING_SNAKE.
var upperName = regexp.MustCompile(`\b[A-Z][A-Z0-9_]+\b`)

// Rust: as compatgen has always read Codex.
var (
	rustAssigned = regexp.MustCompile(`(?:^|[\s,({])([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\s*=[^=>]`)

	rust = &language{
		exts:     []string{".rs"},
		testFile: rustTestFile,
		fnStart:  regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+\w`).MatchString,
		// "const TOOL_CALL_METRIC: &str = "..."": the code that records a surface often names
		// it by the constant.
		constName: regexp.MustCompile(`\bconst\s+([A-Z][A-Z0-9_]*)\s*:\s*&(?:'static\s+)?str\s*=\s*"([^"]*)"`),
		constHead: regexp.MustCompile(`\bconst\s+([A-Z][A-Z0-9_]*)\s*:\s*&(?:'static\s+)?str\s*=\s*$`),
		constant:  []*regexp.Regexp{upperName},
		imported: func(t string, _ *bool) bool {
			return strings.HasPrefix(t, "use ") || strings.HasPrefix(t, "pub use ") || strings.HasPrefix(t, "pub(crate) use ")
		},
		fields: func(line string) []string {
			if !strings.Contains(line, "=") {
				return nil
			}
			var out []string
			for _, m := range rustAssigned.FindAllStringSubmatchIndex(line, -1) {
				before := strings.Fields(line[:m[2]])
				if len(before) > 0 && slices.Contains([]string{"let", "mut", "const", "static", "type"}, before[len(before)-1]) {
					continue
				}
				out = append(out, line[m[2]:m[3]])
			}
			return out
		},
		testModules: true,
	}
)

func rustTestFile(file string) bool {
	base := path.Base(file)
	return strings.Contains(file, "/tests/") || strings.Contains(file, "/benches/") || base == "tests.rs" ||
		strings.HasSuffix(base, "_test.rs") || strings.HasSuffix(base, "_tests.rs")
}

// TypeScript and JavaScript.
var (
	tsKey     = regexp.MustCompile(`(?:^|[\s,{(])([A-Za-z_$][\w$]*)\??\s*:(?:[^:]|$)`)
	tsEnv     = regexp.MustCompile(`\b(?:process|Bun|import\.meta|Deno)\.env\.([A-Za-z_]\w*)`)
	tsMember  = regexp.MustCompile(`\b[A-Z][\w$]*\.[A-Za-z_$][\w$]*`)
	tsEnum    = regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)`)
	tsEnumVal = regexp.MustCompile(`(?:^|[\s{,])([A-Za-z_$][\w$]*)\s*=\s*"([^"]*)"`)

	typescript = &language{
		exts:     []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"},
		skip:     []string{".d.ts", ".d.mts", ".d.cts", ".min.js"},
		testFile: tsTestFile,
		fnStart:  tsFunction,
		// export const EVENT_API_REQUEST = "gemini_cli.api_request" (as const);
		constName: regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Z][A-Z0-9_]*)\s*(?::\s*[^=]+)?=\s*"([^"]*)"\s*(?:as\s+const\s*)?;?\s*$`),
		constHead: regexp.MustCompile(`^\s*(?:export\s+)?const\s+([A-Z][A-Z0-9_]*)\s*(?::\s*[^=]+)?=\s*$`),
		constant:  []*regexp.Regexp{upperName, tsMember},
		imported:  tsImported,
		fields: func(line string) []string {
			var out []string
			for _, m := range tsKey.FindAllStringSubmatch(line, -1) {
				out = append(out, m[1])
			}
			return out
		},
		quoted: func(line string) []string {
			var out []string
			for _, m := range tsEnv.FindAllStringSubmatch(line, -1) {
				out = append(out, m[1])
			}
			return out
		},
		enums: true,
	}
)

var (
	// function x(, async function x(, export default function (, and a class, whose
	// methods are functions of their own.
	tsFunctionWord = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?(?:function\b|class\s)`)
	// const x = (…) =>, const x = async y =>, const x = function
	tsArrow = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+[\w$]+\s*(?::[^=]+)?=\s*(?:async\s+)?(?:function\b|.*=>)`)
	// a class property that holds a function: x = async (…) =>
	tsProperty = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|readonly)\s+)*#?[\w$]+\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)|[\w$]+)\s*(?::\s*[^=]+)?=>`)
	// a method whose parameters close on its line: x(…) {, async x(…): T {, get x() {
	tsMethod = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|async|override|readonly|abstract|get|set)\s+)*(?:\*\s*)?#?([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\s*\(.*\)\s*(?::\s*[^;=]+)?\{\s*$`)
	// one whose parameters run on: a modifier says it is no call (async x(), or the line
	// that closes them names a return type, which no if (…) does.
	tsMethodOpen   = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|async|override|get|set)\s+)+#?[A-Za-z_$][\w$]*\s*(?:<[^>]*>)?\s*\($`)
	tsMethodReturn = regexp.MustCompile(`^\s*\)\s*:\s*[^=;]+\{\s*$`)
	tsKeywords     = []string{"if", "for", "while", "switch", "catch", "with", "return", "function", "else", "do", "new", "typeof", "await", "yield", "super", "this"}
)

// tsFunction reports whether a line of TypeScript or JavaScript starts a function.
func tsFunction(line string) bool {
	if tsFunctionWord.MatchString(line) || tsArrow.MatchString(line) || tsProperty.MatchString(line) ||
		tsMethodOpen.MatchString(line) || tsMethodReturn.MatchString(line) {
		return true
	}
	m := tsMethod.FindStringSubmatch(line)
	return m != nil && !slices.Contains(tsKeywords, m[1])
}

// tsImported reports whether a line imports or re-exports names: import … from, export { … }
// from, and the lines of one across several.
func tsImported(t string, in *bool) bool {
	if *in {
		if strings.Contains(t, "}") {
			*in = false
		}
		return true
	}
	if strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "import{") || strings.HasPrefix(t, "export {") ||
		strings.HasPrefix(t, "export type {") || strings.HasPrefix(t, "export *") {
		*in = strings.Contains(t, "{") && !strings.Contains(t, "}")
		return true
	}
	return false
}

func tsTestFile(file string) bool {
	file = "/" + file
	base := path.Base(file)
	for _, dir := range []string{"/__tests__/", "/__mocks__/", "/__fixtures__/", "/test/", "/tests/", "/e2e/", "/fixtures/",
		"/testFixtures/", "/test-fixtures/", "/mocks/", "/test-utils/", "/testing/", "/integration/", "/integration-tests/", "/evals/"} {
		if strings.Contains(file, dir) {
			return true
		}
	}
	return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.Contains(base, ".e2e.") || strings.Contains(base, ".integration.") ||
		strings.Contains(base, ".stories.") || strings.HasPrefix(base, "test-") || strings.HasSuffix(strings.TrimSuffix(base, path.Ext(base)), "-test")
}

// Python.
var (
	pyKwarg = regexp.MustCompile(`(?:^|[\s,(])([A-Za-z_]\w*)=(?:[^=]|$)`)

	python = &language{
		exts:     []string{".py"},
		testFile: pyTestFile,
		// def and async def, and a class, whose methods are functions of their own.
		fnStart: regexp.MustCompile(`^\s*(?:(?:async\s+)?def|class)\s+\w`).MatchString,
		// A module's TOOL_NAME = "read_file", at the top level only: a class's or a function's
		// is not the module's name for it.
		constName: regexp.MustCompile(`^([A-Z][A-Z0-9_]*)\s*(?::\s*[^=]+)?=\s*"([^"]*)"\s*$`),
		constant:  []*regexp.Regexp{upperName},
		imported:  pyImported,
		// A keyword argument (span(name=…)) as PEP 8 writes one, not an assignment
		// (name = …).
		fields: func(line string) []string {
			var out []string
			for _, m := range pyKwarg.FindAllStringSubmatch(line, -1) {
				out = append(out, m[1])
			}
			return out
		},
	}
)

// pyImported reports whether a line imports names: import x, from x import y, and the lines
// of a parenthesized import.
func pyImported(t string, in *bool) bool {
	if *in {
		if strings.Contains(t, ")") {
			*in = false
		}
		return true
	}
	if strings.HasPrefix(t, "import ") || (strings.HasPrefix(t, "from ") && strings.Contains(t, " import")) {
		*in = strings.HasSuffix(t, "(")
		return true
	}
	return false
}

func pyTestFile(file string) bool {
	file = "/" + file
	base := path.Base(file)
	return strings.Contains(file, "/tests/") || strings.Contains(file, "/test/") || strings.Contains(file, "/testing/") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "conftest.py"
}
