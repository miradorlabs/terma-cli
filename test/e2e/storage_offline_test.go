package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The storage walker records what a turn wrote, by kind, its paths with the run's literals in
// placeholders, and leaves out what was there before and what the sandbox owns.
func TestStorageShapesOffline(t *testing.T) {
	dir := t.TempDir()
	config, home := filepath.Join(dir, "claude"), filepath.Join(dir, "home")
	session := "6f1c2a8e-0b4d-4e8f-9a7b-1c2d3e4f5a6b"
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(config, "settings.json"), `{"env":{}}`)
	write(filepath.Join(config, "untouched.json"), `{"a":1}`)
	opts := StorageOptions{
		Roots:   []StorageRoot{{config, "<claude-config>"}, {home, "<home>"}},
		Replace: map[string]string{session: "<session>", "-work-repo": "<project>"},
		Exclude: []string{"<claude-config>/settings.json"},
		Kinds:   []StorageKind{{regexp.MustCompile(`^<claude-config>/projects/.*\.jsonl$`), "transcript"}},
	}
	before := SnapshotFiles(opts.Roots...)
	// What the turn writes, a moment later so a rewritten file's time moves.
	time.Sleep(10 * time.Millisecond)
	write(filepath.Join(config, "settings.json"), `{"env":{"X":"1"}}`)
	write(filepath.Join(config, "projects", "-work-repo", session+".jsonl"),
		`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`+"\n"+
			`{"type":"assistant","uuid":"a1","message":{"model":"m","usage":{"input_tokens":1}}}`+"\n"+
			`{"type":"user","sessionId":"`+session+`"}`+"\n")
	flags := map[string]any{}
	for i := range 40 {
		flags[fmt.Sprintf("flag_%d", i)] = map[string]any{"on": true}
	}
	write(filepath.Join(config, ".claude.json"), `{"numStartups":1,"projects":{"`+dir+`/repo":{"allowedTools":[]}},"flags":`+mustJSON(flags)+`}`)
	write(filepath.Join(home, ".codex", "sessions", "2026", "11", "04", "rollout-2026-11-04T10-11-12-"+session+".jsonl"),
		`{"timestamp":"t","type":"session_meta","payload":{"id":"x"}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"assistant"}}`+"\n")
	write(filepath.Join(home, ".codex", "config.toml"), "model = \"m\"\n[otel]\nexporter = \"none\"\n[projects.\"/work/repo\"]\ntrust_level = \"trusted\"\n")
	write(filepath.Join(home, ".hermes", "profile.yaml"), "model:\n  default: m\n  provider: custom\n")
	write(filepath.Join(home, ".omp", "logs", "omp.2026-11-04.57033.log"), "started\n")
	write(filepath.Join(home, ".gemini", "tmp", "abcdef0123456789abcdef", "state"), "x")

	shapes := StorageShapes(before, opts)
	if s := shapes["transcript"]; s == nil || s.Path != "<claude-config>/projects/<project>/<session>.jsonl" || s.Format != "jsonl" || s.Files != 1 {
		t.Fatalf("transcript = %+v", s)
	} else {
		if got := s.RecordTypes["user"]; !slices.Equal(got, []string{"message", "message.content", "message.content[].text", "message.content[].type", "message.role", "sessionId", "type", "uuid"}) {
			t.Errorf("user record = %v", got)
		}
		if got := s.RecordTypes["assistant"]; !slices.Contains(got, "message.usage.input_tokens") {
			t.Errorf("assistant record = %v", got)
		}
	}
	// A map of more keys than a record has is one "*".
	if s := shapes["claude"]; s == nil || s.Format != "json" || !slices.Contains(s.Keys, "flags.*.on") || slices.Contains(s.Keys, "flags.flag_1") {
		t.Errorf(".claude.json = %+v", s)
	}
	if s := shapes["sessions"]; s == nil || s.Path != "<home>/.codex/sessions/<n>/<n>/<n>/rollout-<time>-<session>.jsonl" ||
		s.RecordTypes["response_item/message"] == nil || s.RecordTypes["session_meta"] == nil {
		t.Errorf("rollout = %+v", s)
	}
	if s := shapes["settings"]; s == nil || s.Format != "toml" || !slices.Equal(s.Keys, []string{"model", "otel", "otel.exporter", "projects./work/repo", "projects./work/repo.trust_level"}) {
		t.Errorf("config.toml = %+v", s)
	}
	if s := shapes["profile"]; s == nil || s.Format != "yaml" || !slices.Equal(s.Keys, []string{"model", "model.default", "model.provider"}) {
		t.Errorf("profile.yaml = %+v", s)
	}
	if s := shapes["logs"]; s == nil || s.Path != "<home>/.omp/logs/omp.<date>.<n>.log" {
		t.Errorf("logs = %+v", s)
	}
	if s := shapes["tmp"]; s == nil || s.Path != "<home>/.gemini/tmp/<hash>/state" {
		t.Errorf("tmp = %+v", s)
	}
	for kind, s := range shapes {
		if strings.Contains(s.Path, "settings.json") || strings.Contains(s.Path, "untouched") {
			t.Errorf("%s recorded a file the turn did not write: %s", kind, s.Path)
		}
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// A SQLite database is recorded with its tables and columns, its journals beside it.
func TestStorageSQLiteOffline(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
	dir := t.TempDir()
	opts := StorageOptions{Roots: []StorageRoot{{dir, "<codex-home>"}}}
	before := SnapshotFiles(opts.Roots...)
	db := filepath.Join(dir, "state_5.sqlite")
	if out, err := exec.Command("sqlite3", db, "CREATE TABLE threads (id TEXT, title TEXT); CREATE TABLE jobs (n INTEGER);").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	s := StorageShapes(before, opts)["state"]
	if s == nil || s.Format != "sqlite" || !slices.Equal(s.Tables["threads"], []string{"id", "title"}) || !slices.Equal(s.Tables["jobs"], []string{"n"}) {
		t.Fatalf("state = %+v", s)
	}
}

func TestStorageKindOffline(t *testing.T) {
	for pattern, want := range map[string]string{
		"<codex-home>/logs_2.sqlite-wal":                          "logs",
		"<home>/.hermes/state.db":                                 "state",
		"<home>/.local/share/opencode/snapshot/<hash>/HEAD":       "snapshot",
		"<home>/.config/opencode/.gitignore":                      "gitignore",
		"<claude-config>/history.jsonl":                           "history",
		"<home>/.omp/agent/sessions/x/.y.jsonl.lock.os":           "locks",
		"<home>/.cache/opencode/models.json":                      "cache",
		"<claude-config>/todos/<uuid>.json":                       "todos",
		"<codex-home>/installation_id":                            "installation_id",
		"<home>/.gemini/projects.json":                            "projects",
		"<claude-config>/shell-snapshots/snapshot-zsh-<n>-x.sh":   "shell-snapshots",
		"<home>/.dsh/storages/session_projcache/sessions/s.json":  "cache",
		"<repo>/.claude/settings.local.json":                      "settings",
		"<home>/.pi/agent/auth.json":                              "agent",
		"<home>/Library/Application Support/x/Local Storage/y.db": "y",
	} {
		if got := storageKind(pattern); got != want {
			t.Errorf("storageKind(%s) = %q, want %q", pattern, got, want)
		}
	}
}

func TestFileFormatOffline(t *testing.T) {
	for _, c := range []struct{ name, data, want string }{
		{"a.jsonl", `{"a":1}`, "jsonl"},
		{"a.json", `{"a":1}`, "json"},
		{"history", "{\"a\":1}\n{\"b\":2}\n", "jsonl"},
		{"x.db", "SQLite format 3\x00rest", "sqlite"},
		{"x.db-wal", "\x37\x7f\x06\x82", "sqlite-wal"},
		{"c.toml", "a = 1\n", "toml"},
		{"c.yml", "a: 1\n", "yaml"},
		{"r.md", "# r\n", "markdown"},
		{"id", "abc\n", "text"},
		{"blob", "\xff\xfe\x00", "binary"},
	} {
		if got := fileFormat(c.name, []byte(c.data)); got != c.want {
			t.Errorf("fileFormat(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// Normalization replaces the run's literals, ids, times and long numbers, and a harness's own
// rewrites.
func TestStorageNormalizeOffline(t *testing.T) {
	opts := StorageOptions{Replace: map[string]string{"/tmp/sb/repo": "<repo>"},
		Rewrite: []StorageRewrite{{regexp.MustCompile(`^(session-<time>-)[0-9a-f]{8}(\.json)$`), "${1}<id>${2}"}}}
	for in, want := range map[string]string{
		"rollout-2026-11-04T10-11-12-019a4e2b-7c1d-7e3f-8a9b-0c1d2e3f4a5b.jsonl": "rollout-<time>-<uuid>.jsonl",
		"session-2026-11-04T04-36-b8cb315b.json":                                 "session-<time>-<id>.json",
		"2026-11-04T04-36-12-726Z_x.jsonl":                                       "<time>_x.jsonl",
		"omp.2026-11-04.57033.log":                                               "omp.<date>.<n>.log",
		"/tmp/sb/repo":                                                           "<repo>",
		"2026":                                                                   "<n>",
		"state_5.sqlite":                                                         "state_5.sqlite",
	} {
		if got := opts.normalize(in); got != want {
			t.Errorf("normalize(%s) = %q, want %q", in, got, want)
		}
	}
}
