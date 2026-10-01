package output

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"unicode/utf8"
)

const esc = 0x1b // ANSI escape introducer

func TestSanitizeTerminal_StripsControlSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is untouched", "checkout-service", "checkout-service"},
		{"unicode is preserved", "café — 日本語 🚀", "café — 日本語 🚀"},
		{"ANSI colour escape is dropped", "\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"OSC clipboard hijack is dropped", "x\x1b]52;c;Zm9v\x07y", "x]52;c;Zm9vy"},
		{"embedded newline and tab are dropped", "line1\nline2\tcol", "line1line2col"},
		{"C1 control is dropped", "a\u009bb", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeTerminal(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeTerminal(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsRune(got, esc) {
				t.Errorf("result still contains ESC: %q", got)
			}
		})
	}
}

func TestTruncate_IsRuneAware(t *testing.T) {
	// A byte-slicing truncation would cut a multi-byte rune in half and emit invalid
	// UTF-8. The result must always stay valid UTF-8.
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"ascii under limit", "short", 40},
		{"ascii over limit", "a-very-long-service-name", 10},
		{"multibyte over limit", "日本語のサービス名前です", 5},
		{"emoji over limit", "🚀🚀🚀🚀🚀🚀", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.in, tc.max)
			if !utf8.ValidString(got) {
				t.Errorf("Truncate(%q, %d) = %q is not valid UTF-8", tc.in, tc.max, got)
			}
			if utf8.RuneCountInString(tc.in) <= tc.max {
				if got != tc.in {
					t.Errorf("Truncate(%q, %d) = %q, want it unchanged", tc.in, tc.max, got)
				}
			} else if n := utf8.RuneCountInString(got); n != tc.max {
				t.Errorf("Truncate(%q, %d) = %q has %d runes, want %d (incl. the ellipsis)", tc.in, tc.max, got, n, tc.max)
			}
		})
	}
}

func TestRenderTable_SanitizesCells(t *testing.T) {
	var buf bytes.Buffer
	table := Table{
		Headers: []string{"NAME", "BODY"},
		Rows:    [][]string{{"svc", "hello\x1b]0;pwned\x07world"}},
	}
	if err := Render(&buf, FormatTable, table, nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.ContainsRune(buf.String(), esc) {
		t.Errorf("table output leaked an ESC sequence: %q", buf.String())
	}
}

func TestRenderCSV_KeepsFieldsVerbatim(t *testing.T) {
	// CSV is a data export, not a terminal view: a field with an embedded newline or
	// tab must survive verbatim (encoding/csv quotes it), not be stripped the way the
	// human table path sanitizes control characters.
	var buf bytes.Buffer
	body := "line1\nline2\twith tab"
	table := Table{Headers: []string{"NAME", "BODY"}, Rows: [][]string{{"svc", body}}}
	if err := Render(&buf, FormatCSV, table, nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("re-parse CSV: %v", err)
	}
	if len(rows) != 2 || rows[1][1] != body {
		t.Errorf("CSV body = %q, want it preserved verbatim (%q)", rows, body)
	}
}

func TestRenderJSON_LeavesEscapingToTheEncoder(t *testing.T) {
	// JSON is a machine format: the encoder escapes control characters to \uXXXX
	// itself, so the raw ESC never reaches the terminal and the value still
	// round-trips. The sanitizer must not touch this path.
	var buf bytes.Buffer
	data := map[string]string{"body": "a\x1bb"}
	if err := Render(&buf, FormatJSON, Table{}, data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if strings.ContainsRune(out, esc) {
		t.Errorf("JSON output contains a raw ESC byte: %q", out)
	}
	if !strings.Contains(out, "u001b") {
		t.Errorf("JSON should encode the ESC as a \\u001b escape, got %q", out)
	}
}
