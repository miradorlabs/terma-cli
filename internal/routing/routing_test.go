package routing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	testProjectID = "770e8400-e29b-41d4-a716-446655440000"
	testEndpoint  = "https://otel-dev.example.com"
)

// sandbox points config.Dir() at a temporary directory so a test never reads or writes
// real state.
func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestRecordRoundTrip(t *testing.T) {
	sandbox(t)
	in := Record{ProjectID: testProjectID, Endpoint: testEndpoint, Signals: []string{"traces", "logs"}, Harnesses: []string{"claude", "codex"}}
	if err := SaveRecord(in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadRecord(testProjectID)
	if err != nil || !ok {
		t.Fatalf("LoadRecord: ok=%v err=%v", ok, err)
	}
	if got.Endpoint != testEndpoint || len(got.Harnesses) != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestRecordRejectsUnsafeProjectID(t *testing.T) {
	sandbox(t)
	if err := SaveRecord(Record{ProjectID: "../escape"}); err == nil {
		t.Fatal("expected an unsafe project id to be rejected")
	}
}

// A record written by 0.0.2 has no cli field, and today's router reads that as "do not
// route the Codex CLI". The migration restores what the record meant, once, and touches
// nothing else.
func TestMigrateCodexCLIRoutesFillsInWhatOldRecordsMeant(t *testing.T) {
	sandbox(t)
	if err := MigrateCodexCLIRoutes(context.Background()); err != nil {
		t.Fatalf("no routing directory: %v", err)
	}
	dir, err := RoutingDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("a.json", `{"project_id":"a","endpoint":"https://otel","signals":["logs"],"include_prompts":false,"include_tool_content":true,"harnesses":["claude","codex"],"future":{"kept":1}}`)
	claudeOnly := write("b.json", `{"project_id":"b","harnesses":["claude"]}`)
	current := write("c.json", `{"project_id":"c","harnesses":["codex"],"cli":false,"desktop":true}`)
	broken := write("d.json", `{not json`)

	if err := MigrateCodexCLIRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	read := func(p string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return m
	}
	a := read(old)
	if a["cli"] != true || a["desktop"] != false || a["include_prompts"] != false || a["include_tool_content"] != true || a["future"] == nil {
		t.Fatalf("migrated record %v", a)
	}
	if b := read(claudeOnly); b["cli"] != false {
		t.Fatalf("a record that does not route codex: %v", b)
	}
	if c := read(current); c["cli"] != false || c["desktop"] != true {
		t.Fatalf("a record that already says was changed: %v", c)
	}
	if data, _ := os.ReadFile(broken); string(data) != `{not json` {
		t.Fatal("an unparseable record was rewritten")
	}
	if info, _ := os.Stat(old); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	rec, ok, err := LoadRecord("a")
	if err != nil || !ok || !rec.CLI || rec.Desktop {
		t.Fatalf("LoadRecord after migration: %+v %v %v", rec, ok, err)
	}
	// A start whose bound is already spent changes nothing.
	pending := write("e.json", `{"project_id":"e","harnesses":["codex"]}`)
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := MigrateCodexCLIRoutes(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if e := read(pending); e["cli"] != nil {
		t.Fatalf("a cancelled run migrated a record: %v", e)
	}
	before, _ := os.ReadFile(old)
	if err := MigrateCodexCLIRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(old); string(after) != string(before) {
		t.Fatal("a second run changed a migrated record")
	}
}
