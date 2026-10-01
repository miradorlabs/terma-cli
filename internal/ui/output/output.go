// Package output renders command results as a table for a person, and by default as
// JSON for a pipe or an agent, which almost always wants to parse them.
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

	"github.com/miradorlabs/terma-cli/internal/ui/style"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// Format is how a command renders its result: what --output accepts.
type Format string

// The output formats; Table is for a person at a terminal.
const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatCSV   Format = "csv"
)

// AgentMode reports whether the CLI is being driven by a coding agent.
func AgentMode() bool { return style.AgentMode() }

// Interactive reports whether stdout is a terminal a human is likely watching.
func Interactive() bool {
	if AgentMode() {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// Resolve turns the --output flag into a format; unset, a non-terminal or agent caller gets JSON.
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

// Table is the shape every list command produces, its cells already rendered.
type Table struct {
	Headers []string
	Rows    [][]string
}

// Render writes the payload; JSON and YAML serialize the whole of data, not the table.
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
	// CSV cells pass through verbatim: encoding/csv quotes them, and sanitizing is the table's.
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

// KeyValues renders labelled fields for a person, the whole object for machines.
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

// Truncate shortens s to limit runes with an ellipsis, never splitting a rune.
func Truncate(s string, limit int) string {
	if limit <= 1 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-1]) + "…"
}

// SanitizeTerminal drops C0/C1 control characters from a value bound for a terminal:
// ingested telemetry could carry escapes that rewrite the screen or drive the clipboard.
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

// TildePath abbreviates the home directory to ~, as a shell prompt would.
func TildePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// And joins names as prose: "a", "a and b", "a, b and c".
func And(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
