// Package output renders command results in the format the caller asked for.
//
// The default is a human table, but the CLI flips to JSON automatically when it is
// not attached to a terminal or when an agent harness is detected — a piped or
// agent-driven invocation almost always wants to parse the result, and silently
// getting column-aligned text is the most common way that goes wrong.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// Format is how a command renders its result: what --output accepts.
type Format string

// The output formats. Table is for a person at a terminal; the others are for whatever
// reads the command's output instead, and carry the same rows and columns.
const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatCSV   Format = "csv"
)

// agentEnvVars are set by the coding agents that shell out to CLIs. Their presence
// means the consumer is a model, not a person reading a terminal.
var agentEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE",
	"CURSOR_TRACE_ID",
	"CLINE_ACTIVE",
	"GITHUB_COPILOT_CLI",
	"AIDER_MODEL",
}

// AgentMode reports whether the CLI is being driven by an agent harness.
func AgentMode() bool {
	for _, key := range agentEnvVars {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// Interactive reports whether stdout is a terminal a human is likely watching.
func Interactive() bool {
	if AgentMode() {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// Resolve turns the --output flag into a concrete format. An explicit flag always
// wins; otherwise a non-terminal or agent caller gets JSON.
func Resolve(flag string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "":
		if Interactive() {
			return FormatTable, nil
		}
		return FormatJSON, nil
	case "table":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	case "csv":
		return FormatCSV, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want table, json, yaml, or csv)", flag)
	}
}

// Table is the shape every list command produces. Rows carry pre-rendered strings so
// each command decides its own formatting once, rather than the renderer guessing.
type Table struct {
	Headers []string
	Rows    [][]string
}

// Render writes the payload. JSON and YAML serialize `data` — the full object,
// including fields the table omits — so scripting is never limited to what the
// human view happens to show.
func Render(w io.Writer, format Format, table Table, data any) error {
	switch format {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	case FormatYAML:
		enc := yaml.NewEncoder(w)
		defer func() { _ = enc.Close() }()
		return enc.Encode(data)
	case FormatCSV:
		return renderCSV(w, table)
	default:
		return renderTable(w, table)
	}
}

func renderTable(w io.Writer, table Table) error {
	if len(table.Rows) == 0 {
		fmt.Fprintln(w, "No results.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	if len(table.Headers) > 0 {
		fmt.Fprintln(tw, strings.Join(sanitizeRow(table.Headers), "\t"))
	}
	for _, row := range table.Rows {
		fmt.Fprintln(tw, strings.Join(sanitizeRow(row), "\t"))
	}
	return tw.Flush()
}

func renderCSV(w io.Writer, table Table) error {
	// CSV is a data format, not a terminal view: encoding/csv already quotes embedded
	// newlines and commas, so cells pass through verbatim to keep the export faithful.
	// Terminal-escape sanitizing belongs to the human table path, not here.
	cw := csv.NewWriter(w)
	if len(table.Headers) > 0 {
		if err := cw.Write(table.Headers); err != nil {
			return err
		}
	}
	if err := cw.WriteAll(table.Rows); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// KeyValues renders a detail view: a fixed set of labelled fields for humans, the
// whole object for machines.
func KeyValues(w io.Writer, format Format, pairs [][2]string, data any) error {
	if format != FormatTable {
		rows := make([][]string, 0, len(pairs))
		for _, p := range pairs {
			rows = append(rows, []string{p[0], p[1]})
		}
		return Render(w, format, Table{Headers: []string{"field", "value"}, Rows: rows}, data)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range pairs {
		fmt.Fprintf(tw, "%s:\t%s\n", SanitizeTerminal(p[0]), SanitizeTerminal(p[1]))
	}
	return tw.Flush()
}

// Truncate keeps a table column from wrapping and destroying the alignment that
// makes it readable in the first place. It counts and slices by rune, so a limit that
// falls inside a multi-byte character (CJK, emoji) never leaves a mangled half-rune.
func Truncate(s string, limit int) string {
	if limit <= 1 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-1]) + "…"
}

// SanitizeTerminal strips control characters from a value bound for a human-facing
// terminal. Log bodies, trace names, and attribute values come from ingested telemetry
// — data an attacker can influence — and a raw ANSI/OSC escape sequence rendered to a
// terminal can rewrite the screen, retitle the window, or drive the clipboard. Tabs,
// newlines, and other C0/C1 controls are dropped; everything printable, including
// legitimate Unicode, is kept. Machine formats (JSON/YAML/CSV) are deliberately left
// untouched — their encoders escape or quote control characters and their output is
// meant to be parsed, not read off a terminal — so only the table renderer and callers
// printing directly to a terminal route through here.
func SanitizeTerminal(s string) string {
	if !strings.ContainsFunc(s, isControlRune) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControlRune(r) {
			return -1
		}
		return r
	}, s)
}

func isControlRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func sanitizeRow(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = SanitizeTerminal(c)
	}
	return out
}
