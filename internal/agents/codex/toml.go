package codex

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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"

	toml "github.com/pelletier/go-toml/v2"
)

// tomlFile is Codex's config.toml, edited by splicing only the `[otel]` table's text so
// hand-written comments, order and other tables survive byte for byte. A splice that
// changes how anything outside `[otel]` reads is refused, not written.
type tomlFile struct {
	path string
	// writePath is path with symlinks resolved, so a write goes through a symlinked file.
	writePath string
	raw       []byte
	doc       map[string]any
	// otel is the parsed `[otel]` table, never nil; save splices it back.
	otel map[string]any

	existed   bool
	symlinked bool
	mode      fs.FileMode
}

const otelTable = "otel"

func loadTOML(path string) (*tomlFile, error) {
	f := &tomlFile{
		path:      path,
		writePath: path,
		doc:       map[string]any{},
		otel:      map[string]any{},
		mode:      harness.SettingsMode,
	}

	writePath, symlinked, err := harness.ResolveWritePath(path)
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
		// A splice into a file that cannot be parsed would rewrite settings nobody can see.
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, then retry)", path, err)
	}
	if f.doc == nil {
		f.doc = map[string]any{}
	}
	if raw, ok := f.doc[otelTable]; ok {
		table, ok := raw.(map[string]any)
		if !ok {
			// Codex would reject this file itself.
			return nil, fmt.Errorf("parse %s: %q is %s, want a table (fix the file, then retry)",
				path, otelTable, tomlTypeName(raw))
		}
		f.otel = table
	}
	return f, nil
}

// save splices f.otel in place of the current `[otel]` text, dropping an empty table.
// tighten clamps the mode to 0600 once the file carries a credential.
func (f *tomlFile) save(tighten bool) error {
	out, err := spliceOtelTable(f.raw, f.otel)
	if err != nil {
		return fmt.Errorf("rewrite %s: %w", f.path, err)
	}

	// Prove the splice before writing it; a scanner that misjudged a table's bounds fails
	// here and leaves the file untouched.
	if err := verifySplice(f.doc, f.otel, out); err != nil {
		return fmt.Errorf("rewrite %s: %w", f.path, err)
	}

	// An emptied document is removed, unless reached through a symlink, which would dangle.
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
		mode = harness.SettingsMode
	}
	if err := os.MkdirAll(filepath.Dir(f.writePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(f.writePath), err)
	}
	return config.WriteFileAtomic(f.writePath, out, mode)
}

// backup copies the file alongside itself before the first modification; replace lets
// it overwrite a stale backup when the file is not Terma's own work.
func (f *tomlFile) backup(replace bool) (string, error) {
	return harness.BackupFile(f.writePath, f.existed, replace)
}

// verifySplice compares canonical renderings, not reflect.DeepEqual: a datetime parsed
// twice carries two distinct time.Location pointers.
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

// Quoted `["otel"]` matches too; `[otel_extra]` and `[mcp_servers.otel]` must not.
var (
	anyHeaderRE      = regexp.MustCompile(`^\s*\[`)
	otelHeaderRE     = regexp.MustCompile(`^\s*\[\s*(?:otel|"otel"|'otel')\s*(?:\]|\.)`)
	otelRootDottedRE = regexp.MustCompile(`^\s*(?:otel|"otel"|'otel')\s*\.`)
	commentOrBlankRE = regexp.MustCompile(`^\s*(?:#.*)?$`)
)

// spliceOtelTable replaces every piece of text defining the otel table (header, sub-tables,
// root-level `otel.x` keys) with one rendering of otel, preserving the file's line endings.
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
	// Split keeps each trailing "\r" so the join reproduces the file's own endings.
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

	// A root-level dotted key is only removed: a `[otel]` header in its place would swallow
	// every root key after it.
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
				// Blank and comment lines directly before the next header belong to that header.
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
		// A table removed from between two others would leave both separators; keep one.
		if len(out) > 0 && isBlank(out[len(out)-1]) && sp.end < len(lines) && isBlank(lines[sp.end]) {
			out = out[:len(out)-1]
		}
	}
	out = append(out, lines[next:]...)

	if len(otel) > 0 && !inserted {
		if len(out) > 0 && !isBlank(out[len(out)-1]) {
			out = append(out, strings.TrimSuffix(eol, "\n"))
		}
		out = append(out, blockLines...)
		blockEnd = len(out)
	}
	if blockEnd == len(out) {
		trailingNewline = true
	}
	if len(spans) > 0 {
		// A table removed from the very top or bottom leaves a leading or trailing blank line.
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

// renderOtelTable sorts keys so the text is byte-stable across connects.
func renderOtelTable(otel map[string]any) (string, error) {
	if len(otel) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(otel))
	for k := range otel {
		keys = append(keys, k)
	}
	slices.Sort(keys)

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

// renderTOMLValue renders deterministically, so two values that parse the same render the
// same and rendered text can stand in for the value in the ownership journal.
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
		// 2.0 rendered as 2 would read back as an integer.
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
		slices.Sort(keys)
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

// unmarshalTOMLLenient relies on go-toml ignoring keys the struct does not name: the
// files hold far more than the otel table.
func unmarshalTOMLLenient(data []byte, v any) error {
	return toml.Unmarshal(data, v)
}
