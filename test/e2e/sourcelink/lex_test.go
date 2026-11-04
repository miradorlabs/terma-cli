package sourcelink

import (
	"slices"
	"strings"
	"testing"
)

// TypeScript's three quotes are strings, a template literal's text is text and its ${…} code
// (which may hold strings and templates of its own), and its comments, to the end of a line
// or across lines, are neither.
func TestLexTypeScript(t *testing.T) {
	code, text := lexAll("typescript",
		`const a = 'gemini_cli.api_request'; // was "gemini_cli.old"`,
		"const b = \"say 'hi'\" + `tool ${name('read_file')} done`;",
		"/* 'in.comment'",
		"   still */ emit(`x ${`inner ${'deep'}`} y`);",
		"const prompt = `",
		"You are an interactive CLI agent ${'specializing'} in",
		"software engineering tasks.`;",
		`const re = /["']/g; emit('after.regex');`,
		`const half = total / 2; emit('after.division' / 1);`,
		`<p>Don't stop</p>`,
		`emit('next.line');`,
		"const s = 'it\\'s'; const u = \"a // b\";",
	)
	want := []string{
		`const a = "gemini_cli.api_request"; `,
		`const b = "say 'hi'" + "tool " name("read_file") " done";`,
		``,
		` emit("x " "inner " "deep" "" " y");`,
		`const prompt = `,
		// A piece of a template begun on a line before names nothing on this one.
		`"" "specializing" `,
		`"";`,
		`const re = /_/; emit("after.regex");`,
		`const half = total / 2; emit("after.division" / 1);`,
		`<p>Don`,
		`emit("next.line");`,
		`const s = "it\'s"; const u = "a // b";`,
	}
	for i := range want {
		if code[i] != want[i] {
			t.Errorf("line %d code\n got %q\nwant %q", i+1, code[i], want[i])
		}
	}
	if got := text[5]; !slices.Equal(got, []string{"You are an interactive CLI agent ", "specializing", " in"}) {
		t.Errorf("a template literal's line, cut at its interpolation: %q", got)
	}
	if got := text[6]; !slices.Equal(got, []string{"software engineering tasks."}) {
		t.Errorf("a template literal's last line: %q", got)
	}
	if got := text[0]; !slices.Equal(got, []string{"gemini_cli.api_request"}) {
		t.Errorf("a comment's string is no text: %q", got)
	}
}

// Python's strings take prefixes (r, b, f and their pairs), a triple quote spans lines, a
// docstring is text but no code, and # comments are neither.
func TestLexPython(t *testing.T) {
	code, text := lexAll("python",
		`"""Module docstring naming "hermes.not_code"."""`,
		`TOOL = "read_file"  # "in.comment"`,
		`def f():`,
		`    """A function's docstring`,
		`    spanning "lines"."""`,
		`    log(f"hermes.{kind}", rb'\d+', name='x')`,
		`PROMPT = """You are Hermes, an agent`,
		`that helps with "code"."""`,
		`s = 'it\'s' + "#notcomment"`,
		`r = r"\"" + "after.raw"`,
	)
	want := []string{
		``,
		`TOOL = "read_file"  `,
		`def f():`,
		`    `,
		``,
		`    log("hermes.{kind}", "\d+", name="x")`,
		`PROMPT = `,
		`""`,
		`s = "it\'s" + "#notcomment"`,
		`r = "\'" + "after.raw"`,
	}
	for i := range want {
		if code[i] != want[i] {
			t.Errorf("line %d code\n got %q\nwant %q", i+1, code[i], want[i])
		}
	}
	if got := strings.Join(text[6], "|") + "|" + strings.Join(text[7], "|"); got != `You are Hermes, an agent|that helps with "code".` {
		t.Errorf("a triple-quoted string's text: %q", got)
	}
	if got := text[3]; !slices.Equal(got, []string{"A function's docstring"}) {
		t.Errorf("a docstring's text: %q", got)
	}
}

// Rust's code is read as compatgen always has; the text of its strings follows a string, raw
// or not, across lines.
func TestLexRustText(t *testing.T) {
	code, text := lexAll("rust",
		`const D: &str = "Runs a shell command and returns \`,
		`    its output."; // "comment"`,
		`let schema = r#"{"type": "object"}"#;`,
		`let p = r##"You are Codex,`,
		`a coding agent."##;`,
	)
	if code[2] != `let schema = "";` {
		t.Errorf("a raw string is emptied on code: %q", code[2])
	}
	got := [][]string{text[0], text[1], text[2], text[3], text[4]}
	want := [][]string{{"Runs a shell command and returns "}, {"    its output."}, {`{"type": "object"}`}, {"You are Codex,"}, {"a coding agent."}}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("line %d text %q, want %q", i+1, got[i], want[i])
		}
	}
}

// A fragment is a line's text cut where the build fills something in, long enough to place a
// passage.
func TestFragments(t *testing.T) {
	got := fragments("  You are   running in ${cwd} on a {platform} machine, today is %s: be careful with files  ")
	want := []string{"be careful with files"}
	if !slices.Equal(got, want) {
		t.Errorf("fragments = %q, want %q", got, want)
	}
	if got := fragments("You are an interactive CLI agent specializing in software engineering."); len(got) != 1 {
		t.Errorf("a whole line: %q", got)
	}
}
