package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// What a harness build wrote to disk on its turn: a profile scenario snapshots the harness's
// directories before the turn (SnapshotFiles) and, after it, records every file the turn made
// or changed, grouped by kind (RecordStorage): each kind's path pattern, with the run's own
// directories, ids and dates in placeholders, its format, and the shape of what it holds, the
// dotted key paths of each record type of a JSONL transcript, of a JSON or TOML document, or
// the columns of each table of a SQLite database. What the sandbox or terma wrote before the
// turn and the turn left as it was is not the build's; files the sandbox and terma own (the
// hooks configuration they write) are left out by name.

// StorageRoot is a directory a build writes under, and the placeholder its paths take.
type StorageRoot struct{ Dir, Name string }

// FileShape is what a profile records of one kind of file a build wrote.
type FileShape struct {
	// Path is the kind's path pattern; Paths every pattern, where the kind has more than one,
	// the first maxKindPaths of them in order, and MorePaths how many more (a bundle of
	// skills unpacked, a git store).
	Path      string   `json:"path"`
	Paths     []string `json:"paths,omitempty"`
	MorePaths int      `json:"more_paths,omitempty"`
	Format    string   `json:"format"` // jsonl | json | toml | yaml | sqlite | text | markdown | binary
	// RecordTypes are a JSONL file's records by their type (Codex's "type/payload.type"), each
	// with its sorted dotted key paths, three deep, a list's items under "[]".
	RecordTypes map[string][]string `json:"record_types,omitempty"`
	// Keys are a JSON, TOML or YAML document's key paths, the same way.
	Keys []string `json:"keys,omitempty"`
	// Tables are a SQLite database's tables and their columns.
	Tables map[string][]string `json:"tables,omitempty"`
	Files  int                 `json:"files"`
}

type fileStamp struct {
	size int64
	mod  time.Time
}

// FileSnapshot is every file under some roots, by path, as it was.
type FileSnapshot map[string]fileStamp

// SnapshotFiles is what the roots hold now.
func SnapshotFiles(roots ...StorageRoot) FileSnapshot {
	snap := FileSnapshot{}
	for _, r := range roots {
		walkFiles(r.Dir, func(path string, info fs.FileInfo) {
			snap[path] = fileStamp{info.Size(), info.ModTime()}
		})
	}
	return snap
}

// walkFiles calls fn for each regular file under dir, but not in a git directory.
func walkFiles(dir string, fn func(string, fs.FileInfo)) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			fn(path, info)
		}
		return nil
	})
}

// StorageOptions say how one harness's files are told apart.
type StorageOptions struct {
	// Roots are the directories walked, most specific first: a file is named by the first
	// that holds it.
	Roots []StorageRoot
	// Replace maps a literal of this run in a path or key (the session's id, Claude Code's
	// project folder) to its placeholder.
	Replace map[string]string
	// Exclude are path patterns, as normalized, that the sandbox or terma own.
	Exclude []string
	// Kinds name a kind by a pattern's match, before the generic rules (storageKind).
	Kinds []StorageKind
	// Rewrite replaces what else in a segment is the run's own (Gemini's short session id).
	Rewrite []StorageRewrite
}

// StorageRewrite replaces Match in a path segment or key with Replace.
type StorageRewrite struct {
	Match   *regexp.Regexp
	Replace string
}

// StorageKind names the kind of the files whose normalized path matches.
type StorageKind struct {
	Match *regexp.Regexp
	Kind  string
}

// WrittenFiles are the files under opts' roots new or changed since snap, by normalized path
// pattern, excluded ones left out.
func WrittenFiles(snap FileSnapshot, opts StorageOptions) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	for _, r := range opts.Roots {
		walkFiles(r.Dir, func(path string, info fs.FileInfo) {
			if seen[path] {
				return
			}
			seen[path] = true
			if was, ok := snap[path]; ok && was.size == info.Size() && was.mod.Equal(info.ModTime()) {
				return
			}
			pattern := opts.pattern(path)
			if pattern == "" || slices.Contains(opts.Exclude, pattern) {
				return
			}
			out[pattern] = append(out[pattern], path)
		})
	}
	return out
}

// RecordStorage records, in t's profile of b, what b wrote under opts' roots since snap.
func RecordStorage(t *testing.T, b Binary, snap FileSnapshot, opts StorageOptions) {
	t.Helper()
	for kind, shape := range StorageShapes(snap, opts) {
		RecordFile(t, b, kind, shape)
	}
}

// StorageShapes are the shapes of what was written under opts' roots since snap, by kind.
func StorageShapes(snap FileSnapshot, opts StorageOptions) map[string]*FileShape {
	written := WrittenFiles(snap, opts)
	shapes := map[string]*FileShape{}
	for _, pattern := range slices.Sorted(maps.Keys(written)) {
		kind := opts.kind(pattern)
		s := shapes[kind]
		switch {
		case s == nil:
			s = &FileShape{Path: pattern}
			shapes[kind] = s
		case len(s.Paths) >= maxKindPaths:
			s.MorePaths++
		default:
			if len(s.Paths) == 0 {
				s.Paths = []string{s.Path}
			}
			s.Paths = append(s.Paths, pattern)
		}
		for _, path := range written[pattern] {
			s.Files++
			opts.shapeOf(path, s)
		}
	}
	for _, s := range shapes {
		for k, v := range s.RecordTypes {
			s.RecordTypes[k] = sortedUnique(v)
		}
		s.Keys = sortedUnique(s.Keys)
		for k, v := range s.Tables {
			s.Tables[k] = sortedUnique(v)
		}
	}
	return shapes
}

// maxKindPaths is the most path patterns a kind lists.
const maxKindPaths = 25

func sortedUnique(v []string) []string {
	if v == nil {
		return nil
	}
	sort.Strings(v)
	return slices.Compact(v)
}

var (
	uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// A time of day may leave out its seconds (Gemini's "T04-36") or carry milliseconds
	// (omp's "T04-36-12-726Z").
	datetimeRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T_ ]\d{2}[-:.]?\d{2}(?:[-:.]?\d{2})?(?:[.-]\d{1,6})?Z?`)
	dateRE     = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	hexRE      = regexp.MustCompile(`[0-9a-fA-F]{16,}`)
	digitsRE   = regexp.MustCompile(`\d{5,}`) // timestamps, process ids

)

// pattern is path under the first root that holds it, its placeholder first, with the run's
// literals, ids, dates and long numbers in placeholders; "" outside every root.
func (o StorageOptions) pattern(path string) string {
	for _, r := range o.Roots {
		rel, err := filepath.Rel(r.Dir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		segs := strings.Split(filepath.ToSlash(rel), "/")
		for i, s := range segs {
			segs[i] = o.normalize(s)
		}
		return r.Name + "/" + strings.Join(segs, "/")
	}
	return ""
}

// normalize is one path segment or key with the run's literals and generated values replaced.
func (o StorageOptions) normalize(s string) string {
	for _, lit := range slices.SortedFunc(maps.Keys(o.Replace), func(a, b string) int { return len(b) - len(a) }) {
		if lit != "" {
			s = strings.ReplaceAll(s, lit, o.Replace[lit])
		}
	}
	for _, r := range o.Roots {
		s = strings.ReplaceAll(s, r.Dir, r.Name)
	}
	s = uuidRE.ReplaceAllString(s, "<uuid>")
	s = datetimeRE.ReplaceAllString(s, "<time>")
	s = dateRE.ReplaceAllString(s, "<date>")
	s = hexRE.ReplaceAllString(s, "<hash>")
	s = digitsRE.ReplaceAllString(s, "<n>")
	for _, r := range o.Rewrite {
		s = r.Match.ReplaceAllString(s, r.Replace)
	}
	if s != "" && strings.Trim(s, "0123456789") == "" {
		s = "<n>"
	}
	return s
}

// kind is what kind of file pattern names: the harness's own rules, then storageKind.
func (o StorageOptions) kind(pattern string) string {
	for _, k := range o.Kinds {
		if k.Match.MatchString(pattern) {
			return k.Kind
		}
	}
	return storageKind(pattern)
}

// storageKind is the generic kind of a path pattern, by the names harnesses give their files:
// a database by its name ("state" for Codex's state_5.sqlite, its schema version left off), a
// lock, a history, logs, a cache, settings; else the first folder in the harness's own folder
// (a dot folder in the home, or its folder under ~/.config, ~/.local/share, ~/.local/state),
// or the file's name where it sits at the top of one.
func storageKind(pattern string) string {
	_, rel, _ := strings.Cut(pattern, "/")
	base := filepath.Base(rel)
	lower := strings.ToLower(rel)
	stem := func(name string) string {
		name = strings.TrimLeft(name, ".") // a dotfile's name is its stem
		name = strings.TrimSuffix(name, filepath.Ext(name))
		return strings.TrimRight(strings.Trim(strings.ToLower(name), "._-<>"), "0123456789_-")
	}
	switch {
	case dbFileRE.MatchString(base):
		return stem(dbFileRE.FindStringSubmatch(base)[1])
	case strings.Contains(base, ".lock"):
		return "locks"
	case strings.Contains(lower, "history"):
		return "history"
	case strings.HasSuffix(lower, ".log") || strings.Contains(lower, "/log/") || strings.HasPrefix(lower, "log/") ||
		strings.Contains(lower, "logs/") || strings.HasPrefix(lower, "debug/"):
		return "logs"
	case strings.Contains(lower, "cache"):
		return "cache"
	case strings.HasPrefix(base, "settings") || base == "config.toml" || base == "config.json" || base == "config.yaml":
		return "settings"
	}
	parts := strings.Split(rel, "/")
	if strings.HasPrefix(pattern, "<home>/") {
		switch {
		case len(parts) > 3 && parts[0] == ".local" && (parts[1] == "share" || parts[1] == "state"):
			parts = parts[3:]
		case len(parts) > 2 && parts[0] == ".config":
			parts = parts[2:]
		case len(parts) > 1 && strings.HasPrefix(parts[0], "."):
			parts = parts[1:]
		}
	}
	name := parts[0]
	if len(parts) == 1 {
		name = stem(name)
	}
	if name = strings.TrimRight(strings.Trim(strings.ToLower(name), "._-<>"), "0123456789_-"); name == "" {
		return "other"
	}
	return name
}

// dbFileRE matches a database file and its journals, the database's name first.
var dbFileRE = regexp.MustCompile(`^(.+?)\.(?:db|sqlite|sqlite3)(?:-wal|-shm|-journal)?$`)

// maxShapeRead bounds what is read of one file to learn its shape.
const maxShapeRead = 16 << 20

// shapeOf adds what path holds to s: its format and the shape of its records.
func (o StorageOptions) shapeOf(path string, s *FileShape) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	data := make([]byte, maxShapeRead)
	n, _ := f.Read(data)
	for n < len(data) {
		m, err := f.Read(data[n:])
		n += m
		if err != nil || m == 0 {
			break
		}
	}
	_ = f.Close()
	data = data[:n]
	format := fileFormat(path, data)
	if s.Format == "" || s.Format == "text" {
		s.Format = format
	}
	switch format {
	case "jsonl":
		if s.RecordTypes == nil {
			s.RecordTypes = map[string][]string{}
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1<<20), maxShapeRead)
		for sc.Scan() {
			var rec map[string]any
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			typ := recordType(rec)
			s.RecordTypes[typ] = append(s.RecordTypes[typ], o.keyPaths(rec, 3)...)
		}
	case "json":
		var doc any
		if json.Unmarshal(data, &doc) == nil {
			s.Keys = append(s.Keys, o.keyPaths(doc, 3)...)
		}
	case "toml":
		s.Keys = append(s.Keys, o.tomlKeys(data)...)
	case "yaml":
		s.Keys = append(s.Keys, o.yamlKeys(data)...)
	case "sqlite":
		for table, cols := range sqliteTables(path) {
			if s.Tables == nil {
				s.Tables = map[string][]string{}
			}
			s.Tables[table] = append(s.Tables[table], cols...)
		}
	}
}

// fileFormat says what a file holds, by its first bytes and then its name.
func fileFormat(path string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.HasPrefix(data, []byte("SQLite format 3\x00")):
		return "sqlite"
	case strings.HasSuffix(ext, "-wal") || strings.HasSuffix(ext, "-shm") || strings.HasSuffix(ext, "-journal"):
		return "sqlite-" + ext[strings.LastIndexByte(ext, '-')+1:]
	case ext == ".jsonl" || ext == ".ndjson":
		return "jsonl"
	case len(trimmed) > 0 && json.Valid(trimmed):
		return "json"
	case len(trimmed) > 0 && trimmed[0] == '{' && jsonLines(trimmed):
		return "jsonl"
	case ext == ".toml":
		return "toml"
	case ext == ".yaml" || ext == ".yml":
		return "yaml"
	case ext == ".md":
		return "markdown"
	case utf8.Valid(data):
		return "text"
	}
	return "binary"
}

// jsonLines reports whether every line of data is a JSON value.
func jsonLines(data []byte) bool {
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 && !json.Valid(line) {
			return false
		}
	}
	return true
}

// recordType is a JSONL record's type: its type field, and its payload's where it wraps one
// (Codex's rollout: "response_item/message").
func recordType(rec map[string]any) string {
	typ, _ := rec["type"].(string)
	if typ == "" {
		typ = "(untyped)"
	}
	if payload, ok := rec["payload"].(map[string]any); ok {
		if pt, ok := payload["type"].(string); ok && pt != "" {
			typ += "/" + pt
		}
	}
	return typ
}

// maxRecordKeys is the most keys an object has before keyPaths takes it for a map.
const maxRecordKeys = 32

// keyPaths are v's dotted key paths, depth deep: an object's keys, a list's items' under "[]".
// An object of more keys than a record has is a map (feature flags by name, projects by
// path): its keys are one "*", its values' keys merged under it.
func (o StorageOptions) keyPaths(v any, depth int) []string {
	var out []string
	var walk func(v any, prefix string, d int)
	walk = func(v any, prefix string, d int) {
		switch x := v.(type) {
		case map[string]any:
			if len(x) > maxRecordKeys {
				path := "*"
				if prefix != "" {
					path = prefix + ".*"
				}
				out = append(out, path)
				if d+1 < depth {
					for _, item := range x {
						walk(item, path, d+1)
					}
				}
				return
			}
			for k, item := range x {
				path := o.normalize(k)
				if prefix != "" {
					path = prefix + "." + path
				}
				out = append(out, path)
				if d+1 < depth {
					walk(item, path, d+1)
				}
			}
		case []any:
			for _, item := range x {
				if _, ok := item.(map[string]any); ok {
					walk(item, prefix+"[]", d)
				}
			}
		}
	}
	walk(v, "", 0)
	return out
}

var (
	tomlTableRE = regexp.MustCompile(`^\s*\[\[?\s*([^\]]+?)\s*\]\]?\s*(#.*)?$`)
	tomlKeyRE   = regexp.MustCompile(`^\s*("[^"]*"|[A-Za-z0-9_.-]+)\s*=`)
	yamlKeyRE   = regexp.MustCompile(`^(\s*)(?:-\s+)?("[^"]*"|'[^']*'|[A-Za-z0-9_.$-]+)\s*:(\s|$)`)
)

// tomlKeys are a TOML document's key paths, its tables' and theirs, three deep. It reads
// headers and keys line by line, which is all a harness's config files use.
func (o StorageOptions) tomlKeys(data []byte) []string {
	var out []string
	table := ""
	for line := range strings.Lines(string(data)) {
		if m := tomlTableRE.FindStringSubmatch(line); m != nil {
			table = o.dotted(m[1])
			out = append(out, limitDepth(table, 3))
			continue
		}
		if m := tomlKeyRE.FindStringSubmatch(line); m != nil {
			key := o.dotted(m[1])
			if table != "" {
				key = table + "." + key
			}
			out = append(out, limitDepth(key, 3))
		}
	}
	return out
}

// yamlKeys are a YAML document's key paths by indentation, three deep.
func (o StorageOptions) yamlKeys(data []byte) []string {
	var out []string
	type level struct {
		indent int
		key    string
	}
	var stack []level
	for line := range strings.Lines(string(data)) {
		m := yamlKeyRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent := len(m[1])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		key := o.normalize(strings.Trim(m[2], `"'`))
		path := key
		if len(stack) > 0 {
			path = stack[len(stack)-1].key + "." + key
		}
		stack = append(stack, level{indent, path})
		out = append(out, limitDepth(path, 3))
	}
	return out
}

// dotted is a TOML key, quoted parts unquoted and normalized.
func (o StorageOptions) dotted(key string) string {
	var parts []string
	for key != "" {
		key = strings.TrimSpace(key)
		var part string
		if strings.HasPrefix(key, `"`) {
			end := strings.Index(key[1:], `"`)
			if end < 0 {
				part, key = key[1:], ""
			} else {
				part, key = key[1:end+1], strings.TrimPrefix(strings.TrimSpace(key[end+2:]), ".")
			}
		} else {
			part, key, _ = strings.Cut(key, ".")
		}
		parts = append(parts, o.normalize(strings.TrimSpace(part)))
	}
	return strings.Join(parts, ".")
}

func limitDepth(path string, depth int) string {
	parts := strings.SplitN(path, ".", depth+1)
	if len(parts) > depth {
		parts = parts[:depth]
	}
	return strings.Join(parts, ".")
}

// sqliteTables are a SQLite database's tables and their columns, as the sqlite3 command reads
// them; none where it is not installed.
func sqliteTables(path string) map[string][]string {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sqlite3", "-readonly", "-separator", "\t", path,
		"SELECT m.name, p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type = 'table' ORDER BY m.name, p.cid").Output()
	if err != nil {
		return nil
	}
	tables := map[string][]string{}
	for line := range strings.Lines(string(out)) {
		if table, col, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t"); ok {
			tables[table] = append(tables[table], col)
		}
	}
	return tables
}
