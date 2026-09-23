package harness

import (
	"strings"
	"testing"
)

func mustSplice(t *testing.T, raw string, otel map[string]any) string {
	t.Helper()
	out, err := spliceOtelTable([]byte(raw), otel)
	if err != nil {
		t.Fatalf("spliceOtelTable: %v", err)
	}
	if err := verifySplice(mustParse(t, raw), otel, out); err != nil {
		t.Fatalf("verifySplice: %v\n--- output ---\n%s", err, out)
	}
	return string(out)
}

func mustParse(t *testing.T, raw string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return doc
	}
	if err := unmarshalTOMLLenient([]byte(raw), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

var sampleOtel = map[string]any{
	"log_user_prompt": true,
	"exporter": map[string]any{"otlp-http": map[string]any{
		"endpoint": "https://otel.terma.ai/v1/logs",
		"protocol": "binary",
	}},
}

const sampleBlock = `[otel]
exporter = { otlp-http = { endpoint = "https://otel.terma.ai/v1/logs", protocol = "binary" } }
log_user_prompt = true
`

// The whole point of splicing instead of re-serializing: a hand-written file keeps its
// comments, blank lines and key order, and the new table is simply appended.
func TestSpliceAppendsWhenNoOtelTable(t *testing.T) {
	const raw = `# my codex config
model = "gpt-5"   # trailing comment

[mcp_servers.foo]
command = "foo"
args = ["--bar"]
`
	got := mustSplice(t, raw, sampleOtel)
	if want := raw + "\n" + sampleBlock; got != want {
		t.Fatalf("splice =\n%s\nwant\n%s", got, want)
	}
}

func TestSpliceAppendsWithoutTrailingNewline(t *testing.T) {
	got := mustSplice(t, `model = "gpt-5"`, sampleOtel)
	if want := "model = \"gpt-5\"\n\n" + sampleBlock; got != want {
		t.Fatalf("splice =\n%q\nwant\n%q", got, want)
	}
}

func TestSpliceIntoEmptyFile(t *testing.T) {
	if got := mustSplice(t, "", sampleOtel); got != sampleBlock {
		t.Fatalf("splice = %q, want just the block", got)
	}
}

// A table in the middle is replaced in place; the comment that introduces the *next*
// table stays with it, and everything else is untouched byte for byte.
func TestSpliceReplacesTableInPlace(t *testing.T) {
	const raw = `model = "gpt-5"

[otel]
# this comment is inside the table and does not survive
environment = "staging"
exporter = "none"

# MCP servers
[mcp_servers.foo]
command = "foo"
`
	otel := map[string]any{"environment": "staging", "log_user_prompt": true}
	got := mustSplice(t, raw, otel)
	const want = `model = "gpt-5"

[otel]
environment = "staging"
log_user_prompt = true

# MCP servers
[mcp_servers.foo]
command = "foo"
`
	if got != want {
		t.Fatalf("splice =\n%s\nwant\n%s", got, want)
	}
}

// Sub-tables and root-level dotted keys are all the same table to TOML, and would
// collide with a fresh `[otel]` header. Every piece is folded into the one block.
func TestSpliceFoldsSubtablesAndDottedKeys(t *testing.T) {
	const raw = `otel.environment = "prod"
model = "gpt-5"

[otel.exporter.otlp-http]
endpoint = "https://other.example.com/v1/logs"
protocol = "binary"

[profiles.fast]
model = "gpt-5-mini"

["otel".span_attributes]
"team" = "payments"
`
	otel := map[string]any{"environment": "prod", "log_user_prompt": false}
	got := mustSplice(t, raw, otel)
	const want = `model = "gpt-5"

[otel]
environment = "prod"
log_user_prompt = false

[profiles.fast]
model = "gpt-5-mini"
`
	if got != want {
		t.Fatalf("splice =\n%s\nwant\n%s", got, want)
	}
}

// Names that merely start with "otel" belong to somebody else.
func TestSpliceDoesNotMatchLookalikeTables(t *testing.T) {
	const raw = `[otel_extra]
x = 1

[mcp_servers.otel]
command = "otel-thing"

[otelx.y]
z = 2
`
	got := mustSplice(t, raw, sampleOtel)
	if !strings.HasPrefix(got, raw) {
		t.Fatalf("a lookalike table was touched:\n%s", got)
	}
}

// An emptied table disappears without leaving a doubled blank line or a bare header.
func TestSpliceRemovesEmptyTable(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"middle": {
			raw:  "model = \"a\"\n\n[otel]\nexporter = \"none\"\n\n[mcp_servers.x]\ncommand = \"x\"\n",
			want: "model = \"a\"\n\n[mcp_servers.x]\ncommand = \"x\"\n",
		},
		"end": {
			raw:  "model = \"a\"\n\n[otel]\nexporter = \"none\"\n",
			want: "model = \"a\"\n",
		},
		"top": {
			raw:  "[otel]\nexporter = \"none\"\n\n[mcp_servers.x]\ncommand = \"x\"\n",
			want: "[mcp_servers.x]\ncommand = \"x\"\n",
		},
		"only": {
			raw:  "[otel]\nexporter = \"none\"\n",
			want: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mustSplice(t, tc.raw, map[string]any{}); got != tc.want {
				t.Fatalf("splice = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSplicePreservesCRLF(t *testing.T) {
	raw := "model = \"a\"\r\n\r\n[otel]\r\nexporter = \"none\"\r\n"
	got := mustSplice(t, raw, map[string]any{"log_user_prompt": true})
	want := "model = \"a\"\r\n\r\n[otel]\r\nlog_user_prompt = true\r\n"
	if got != want {
		t.Fatalf("splice = %q, want %q", got, want)
	}
}

// The safety net: a scanner mistake is caught by re-parsing rather than written.
func TestVerifySpliceRejectsAChangedDocument(t *testing.T) {
	original := mustParse(t, "model = \"a\"\n")
	if err := verifySplice(original, map[string]any{}, []byte("model = \"b\"\n")); err == nil {
		t.Fatal("a rewrite that changed a setting outside otel was accepted")
	}
	if err := verifySplice(original, map[string]any{"x": int64(1)}, []byte("model = \"a\"\n[otel]\nx = 2\n")); err == nil {
		t.Fatal("a rewrite whose otel table differs from the intended one was accepted")
	}
	if err := verifySplice(original, map[string]any{}, []byte("model = \n")); err == nil {
		t.Fatal("an unparseable rewrite was accepted")
	}
}

func TestRenderTOMLValueRoundTrips(t *testing.T) {
	for _, text := range []string{
		`"plain"`,
		`"with \"quotes\" and \\ backslash and \n newline and \u0001 control"`,
		`true`,
		`42`,
		`-7`,
		`2.0`,
		`1.5e+300`,
		`[]`,
		`[1, 2, 3]`,
		`["a", { b = 1 }]`,
		`{}`,
		`{ "enduser.id" = "dev@example.com", "mirador.project.id" = "proj_123" }`,
		`{ otlp-http = { endpoint = "https://x/v1/logs", headers = { Authorization = "Bearer k" }, protocol = "binary" } }`,
		`1979-05-27T07:32:00Z`,
		`1979-05-27`,
	} {
		v, err := parseTOMLValue(text)
		if err != nil {
			t.Fatalf("parse %s: %v", text, err)
		}
		got, err := renderTOMLValue(v)
		if err != nil {
			t.Fatalf("render %s: %v", text, err)
		}
		if got != text {
			t.Errorf("round trip of %s = %s", text, got)
		}
	}
}

// Keys inside an inline table are sorted, so the same value always renders the same
// text — which is what lets rendered text stand in for the value in the journal.
func TestRenderTOMLValueIsCanonical(t *testing.T) {
	a, _ := parseTOMLValue(`{ b = 1, a = 2 }`)
	b, _ := parseTOMLValue(`{ a = 2, b = 1 }`)
	ra, _ := renderTOMLValue(a)
	rb, _ := renderTOMLValue(b)
	if ra != rb || ra != `{ a = 2, b = 1 }` {
		t.Fatalf("renderings differ: %q vs %q", ra, rb)
	}
}
