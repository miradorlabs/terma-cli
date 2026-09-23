package harness

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/config"

	toml "github.com/pelletier/go-toml/v2"
)

// tomlFile is a harness config file whose telemetry lives in a TOML table — the shape
// Codex uses, with everything under `[otel]` in config.toml.
//
// The document is parsed to read it, but it is never re-serialized as a whole. A
// config.toml is hand-written: it carries comments, blank lines, a chosen key order, and
// tables for MCP servers and profiles that a round trip through a parser would flatten
// and reorder. So a write replaces only the text of the `[otel]` table — the lines from
// its header to the next header — and leaves every other byte exactly as found. The
// result is re-parsed before it is written, and refused if anything outside `[otel]`
// reads differently from the original: a splice that could not be proven safe is not
// written at all.
//
// Inside `[otel]` the whole table is rewritten, so comments placed within it do not
// survive a connect. Keys in it that Terma does not own do.
type tomlFile struct {
	path string
	// writePath is path with symlinks resolved, for the same reason settingsFile has one.
	writePath string
	// raw is the file exactly as read; the splice works on this text.
	raw []byte
	// doc is the parsed document, for reading settings outside the otel table.
	doc map[string]any
	// otel is the parsed `[otel]` table, never nil. save writes this back in place of
	// whatever the text currently holds for it.
	otel map[string]any

	existed   bool
	symlinked bool
	mode      fs.FileMode
}

const otelTable = "otel"

// loadTOML reads a config file, tolerating its absence.
func loadTOML(path string) (*tomlFile, error) {
	f := &tomlFile{
		path:      path,
		writePath: path,
		doc:       map[string]any{},
		otel:      map[string]any{},
		mode:      settingsMode,
	}

	writePath, symlinked, err := resolveWritePath(path)
	if err != nil {
		return nil, err
	}
	f.writePath, f.symlinked = writePath, symlinked

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	f.existed = true
	f.raw = data

	if info, statErr := os.Stat(path); statErr == nil {
		f.mode = info.Mode().Perm()
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return f, nil
	}

	if err := toml.Unmarshal(data, &f.doc); err != nil {
		// Refusing here is the whole point: a splice into a file we cannot parse would
		// mean rewriting settings we cannot see.
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, then retry)", path, err)
	}
	if f.doc == nil {
		f.doc = map[string]any{}
	}
	if raw, ok := f.doc[otelTable]; ok {
		table, ok := raw.(map[string]any)
		if !ok {
			// Codex would reject this file itself; say so rather than silently replacing
			// whatever it is with a table.
			return nil, fmt.Errorf("parse %s: %q is %s, want a table (fix the file, then retry)",
				path, otelTable, tomlTypeName(raw))
		}
		f.otel = table
	}
	return f, nil
}

// save writes the document back with f.otel spliced in place of the current `[otel]`
// text. An empty table is removed from the text rather than written as a bare header.
//
// tighten is set when the file now carries a credential; it clamps the mode to 0600
// rather than preserving a permissive one. A file already at 0400 keeps that.
func (f *tomlFile) save(tighten bool) error {
	out, err := spliceOtelTable(f.raw, f.otel)
	if err != nil {
		return fmt.Errorf("rewrite %s: %w", f.path, err)
	}

	// Prove the splice before writing it: everything outside the otel table must read
	// exactly as it did, and the otel table must read as what was asked for. A scanner
	// that misjudged where a table starts or ends would fail here, and the file is left
	// untouched rather than written wrong.
	if err := verifySplice(f.doc, f.otel, out); err != nil {
		return fmt.Errorf("rewrite %s: %w", f.path, err)
	}

	// A document with nothing left in it is removed rather than written empty — unless
	// it is reached through a symlink, where removing the target would leave the link
	// dangling. There, the empty text is written instead.
	if len(bytes.TrimSpace(out)) == 0 && !f.symlinked {
		if !f.existed {
			return nil
		}
		if err := os.Remove(f.writePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", f.writePath, err)
		}
		return nil
	}

	mode := f.mode
	if tighten && mode&0o077 != 0 {
		mode = settingsMode
	}
	if err := os.MkdirAll(filepath.Dir(f.writePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(f.writePath), err)
	}
	return config.WriteFileAtomic(f.writePath, out, mode)
}

// backup copies the file alongside itself before the first modification. See
// settingsFile.backup for the rule behind replace.
func (f *tomlFile) backup(replace bool) (string, error) {
	return backupFile(f.writePath, f.existed, replace)
}

// verifySplice re-parses the spliced text and checks it against what was intended.
// Comparison is by canonical rendering rather than reflect.DeepEqual: a datetime parsed
// twice carries two distinct time.Location pointers and would never compare equal.
func verifySplice(original map[string]any, otel map[string]any, out []byte) error {
	reparsed := map[string]any{}
	if len(bytes.TrimSpace(out)) > 0 {
		if err := toml.Unmarshal(out, &reparsed); err != nil {
			return fmt.Errorf("the rewritten file does not parse (%w); nothing was written", err)
		}
	}

	rest := func(doc map[string]any) map[string]any {
		copied := make(map[string]any, len(doc))
		for k, v := range doc {
			if k != otelTable {
				copied[k] = v
			}
		}
		return copied
	}
	before, err := renderTOMLValue(rest(original))
	if err != nil {
		return err
	}
	after, err := renderTOMLValue(rest(reparsed))
	if err != nil {
		return err
	}
	if before != after {
		return errors.New("the rewrite would change settings outside the otel table; nothing was written")
	}

	got, _ := reparsed[otelTable].(map[string]any)
	if len(got) == 0 && len(otel) == 0 {
		return nil
	}
	wantText, err := renderTOMLValue(otel)
	if err != nil {
		return err
	}
	gotText, err := renderTOMLValue(got)
	if err != nil {
		return err
	}
	if wantText != gotText {
		return errors.New("the rewritten otel table does not read back as intended; nothing was written")
	}
	return nil
}

// Table headers and root-level dotted keys that belong to the otel table. A quoted
// `["otel"]` is accepted as well as the bare spelling; `[otel_extra]` and
// `[mcp_servers.otel]` are not otel's, and must not match.
var (
	anyHeaderRE      = regexp.MustCompile(`^\s*\[`)
	otelHeaderRE     = regexp.MustCompile(`^\s*\[\s*(?:otel|"otel"|'otel')\s*(?:\]|\.)`)
	otelRootDottedRE = regexp.MustCompile(`^\s*(?:otel|"otel"|'otel')\s*\.`)
	commentOrBlankRE = regexp.MustCompile(`^\s*(?:#.*)?$`)
)

// spliceOtelTable returns raw with every piece of text defining the otel table — the
// `[otel]` header and its body, any `[otel.x]` sub-tables, and any root-level `otel.x`
// dotted keys — replaced by a single rendering of otel. When otel is empty the pieces
// are removed and nothing is added. When there was no otel table, one is appended.
//
// Line endings are preserved: existing lines keep theirs, and the inserted block uses
// whichever the file already uses.
func spliceOtelTable(raw []byte, otel map[string]any) ([]byte, error) {
	block, err := renderOtelTable(otel)
	if err != nil {
		return nil, err
	}

	text := string(raw)
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	// Split keeps a trailing "\r" on each line so that the join below reproduces the
	// file's own endings; only pattern matching trims it.
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	trailingNewline := strings.HasSuffix(text, "\n")
	if trailingNewline {
		lines = lines[:len(lines)-1]
	}
	clean := func(line string) string { return strings.TrimRight(line, "\r") }
	isBlank := func(line string) bool { return strings.TrimSpace(clean(line)) == "" }

	// header marks a `[otel…]` block, which is where the rendered block may go. A
	// root-level dotted key is only ever removed: a `[otel]` header written in its place,
	// before the first table header, would swallow every root key after it.
	type span struct {
		start, end int
		header     bool
	}
	var spans []span
	preamble := true
	for i := 0; i < len(lines); {
		line := clean(lines[i])
		if anyHeaderRE.MatchString(line) {
			preamble = false
			if otelHeaderRE.MatchString(line) {
				j := i + 1
				for j < len(lines) && !anyHeaderRE.MatchString(clean(lines[j])) {
					j++
				}
				// Blank and comment lines directly before the next header describe that
				// header, not this table; leave them where they are.
				end := j
				for end > i+1 && commentOrBlankRE.MatchString(clean(lines[end-1])) {
					end--
				}
				spans = append(spans, span{i, end, true})
				i = j
				continue
			}
		} else if preamble && otelRootDottedRE.MatchString(line) {
			spans = append(spans, span{i, i + 1, false})
		}
		i++
	}

	blockLines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	if eol == "\r\n" {
		for k := range blockLines {
			blockLines[k] += "\r"
		}
	}

	var out []string
	inserted := false
	blockEnd := -1
	next := 0
	for _, sp := range spans {
		out = append(out, lines[next:sp.start]...)
		next = sp.end
		if sp.header && !inserted && len(otel) > 0 {
			out = append(out, blockLines...)
			blockEnd = len(out)
			inserted = true
			continue
		}
		// Removed. A table taken out from between two others would leave both their
		// separating blank lines behind; keep one.
		if len(out) > 0 && isBlank(out[len(out)-1]) && sp.end < len(lines) && isBlank(lines[sp.end]) {
			out = out[:len(out)-1]
		}
	}
	out = append(out, lines[next:]...)

	if len(otel) > 0 && !inserted {
		// No otel table header anywhere: append one, separated from what precedes it.
		if len(out) > 0 && !isBlank(out[len(out)-1]) {
			out = append(out, strings.TrimSuffix(eol, "\n"))
		}
		out = append(out, blockLines...)
		blockEnd = len(out)
	}
	if blockEnd == len(out) {
		// The block is the last thing in the file; end it properly even if the original
		// text stopped short of a final newline.
		trailingNewline = true
	}
	if len(spans) > 0 {
		// A table removed from the very top or bottom leaves its separator as a leading
		// or trailing blank line; drop those, and nothing else.
		if spans[0].start == 0 {
			for len(out) > 0 && isBlank(out[0]) {
				out = out[1:]
			}
		}
		if spans[len(spans)-1].end == len(lines) && blockEnd != len(out) {
			for len(out) > 0 && isBlank(out[len(out)-1]) {
				out = out[:len(out)-1]
			}
		}
	}

	joined := strings.Join(out, "\n")
	if trailingNewline && joined != "" {
		joined += "\n"
	}
	return []byte(joined), nil
}

// renderOtelTable renders the table as a `[otel]` block, one key per line in sorted
// order so the text is byte-stable across connects. Values are inline, matching the
// shape Codex's own documentation uses.
func renderOtelTable(otel map[string]any) (string, error) {
	if len(otel) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(otel))
	for k := range otel {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("[" + otelTable + "]\n")
	for _, k := range keys {
		v, err := renderTOMLValue(otel[k])
		if err != nil {
			return "", fmt.Errorf("%s.%s: %w", otelTable, k, err)
		}
		b.WriteString(quoteTOMLKey(k) + " = " + v + "\n")
	}
	return b.String(), nil
}

// renderTOMLValue renders a parsed TOML value back as TOML text, deterministically:
// table keys are sorted, numbers are minimal, strings are basic strings. Two values
// that parse the same render the same, which is what lets rendered text stand in for
// the value in the ownership journal.
func renderTOMLValue(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", errors.New("nil value")
	case string:
		return quoteTOMLString(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float64:
		switch {
		case math.IsNaN(x):
			return "nan", nil
		case math.IsInf(x, 1):
			return "inf", nil
		case math.IsInf(x, -1):
			return "-inf", nil
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		// A float must stay a float: 2.0 rendered as 2 would read back as an integer.
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s, nil
	case time.Time:
		return x.Format(time.RFC3339Nano), nil
	case toml.LocalDate:
		return x.String(), nil
	case toml.LocalDateTime:
		return x.String(), nil
	case toml.LocalTime:
		return x.String(), nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			s, err := renderTOMLValue(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		if len(x) == 0 {
			return "{}", nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			s, err := renderTOMLValue(x[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, quoteTOMLKey(k)+" = "+s)
		}
		return "{ " + strings.Join(parts, ", ") + " }", nil
	default:
		return "", fmt.Errorf("unsupported TOML value of type %s", reflect.TypeOf(v))
	}
}

// parseTOMLValue is the inverse of renderTOMLValue for a single value.
func parseTOMLValue(text string) (any, error) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte("v = "+text), &doc); err != nil {
		return nil, fmt.Errorf("parse TOML value %q: %w", text, err)
	}
	v, ok := doc["v"]
	if !ok {
		return nil, fmt.Errorf("parse TOML value %q: no value", text)
	}
	return v, nil
}

var bareKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func quoteTOMLKey(k string) string {
	if bareKeyRE.MatchString(k) {
		return k
	}
	return quoteTOMLString(k)
}

// quoteTOMLString renders a basic (double-quoted) string with every character TOML
// requires escaped.
func quoteTOMLString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case utf8.RuneError:
			b.WriteString(`�`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlTypeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "a table"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case int64, float64:
		return "a number"
	default:
		return reflect.TypeOf(v).String()
	}
}

// unmarshalTOMLLenient decodes into a struct, ignoring keys the struct does not name.
// go-toml's default is already lenient; the name records that this is relied upon for
// files that hold far more than the otel table.
func unmarshalTOMLLenient(data []byte, v any) error {
	return toml.Unmarshal(data, v)
}
