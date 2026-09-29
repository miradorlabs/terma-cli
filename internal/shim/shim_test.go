package shim

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProjectID = "770e8400-e29b-41d4-a716-446655440000"

// sandbox points config.Dir() and the home directory at temporary directories so a test
// never reads or writes real state.
func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")
}

// writeRecord leaves a routing record the way an earlier terma wrote one.
func writeRecord(t *testing.T, rec Record) {
	t.Helper()
	dir, err := RoutingDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rec.ProjectID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRecordReadsWhatAnEarlierTermaWrote(t *testing.T) {
	sandbox(t)
	if _, ok, err := LoadRecord(testProjectID); ok || err != nil {
		t.Fatalf("no record: ok=%v err=%v", ok, err)
	}
	writeRecord(t, Record{ProjectID: testProjectID, Endpoint: "https://otel", Harnesses: []string{AgentClaude, AgentCodex}})
	got, ok, err := LoadRecord(testProjectID)
	if err != nil || !ok || got.Endpoint != "https://otel" || len(got.Harnesses) != 2 {
		t.Fatalf("LoadRecord = %+v, %v, %v", got, ok, err)
	}
	if _, _, err := LoadRecord("../escape"); err == nil {
		t.Fatal("an unsafe project id must be refused")
	}
}

// A shim an earlier terma left on PATH asks for the arguments to route the agent with.
// There are none now: the answer is the empty plan, and the agent starts unchanged.
func TestPrepareAnswersTheEmptyPlan(t *testing.T) {
	dir := t.TempDir()
	if err := Prepare(AgentCodex, dir, []string{"exec", "hi"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("prepare wrote %d files, want only the count", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || string(data) != "terma-args-v1:0\n" {
		t.Fatalf("count = %q, %v", data, err)
	}
}

func TestRemoveAllTearsDownRoutingState(t *testing.T) {
	sandbox(t)
	binDir, _ := ShimBinDir()
	writeExe(t, filepath.Join(binDir, AgentCodex))
	writeRecord(t, Record{ProjectID: testProjectID, Harnesses: []string{AgentCodex}, CLI: true})
	claude, _ := dir("claude", testProjectID)
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	rc, ok := ShellRC()
	if !ok {
		t.Fatal("no startup file for zsh")
	}
	if _, err := rc.Ensure(binDir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"shim", "routing", "claude"} {
		d, _ := dir(sub)
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s survived: %v", sub, err)
		}
	}
	if data, _ := os.ReadFile(rc.Path); strings.Contains(string(data), binDir) {
		t.Fatalf("the PATH block survived:\n%s", data)
	}
	// RemoveAll on a clean machine is not an error.
	if err := RemoveAll(); err != nil {
		t.Fatalf("RemoveAll twice: %v", err)
	}
}

func TestRealBinarySkipsShimDir(t *testing.T) {
	sandbox(t)
	shimDir, _ := ShimBinDir()
	realDir := t.TempDir()
	// A shim named codex, and a real codex elsewhere.
	writeExe(t, filepath.Join(shimDir, "codex"))
	realCodex := filepath.Join(realDir, "codex")
	writeExe(t, realCodex)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)

	got, err := RealBinary("codex")
	if err != nil {
		t.Fatal(err)
	}
	if got == filepath.Join(shimDir, "codex") {
		t.Fatalf("RealBinary returned the shim, not the real binary: %q", got)
	}
	if resolve(got) != resolve(realCodex) {
		t.Fatalf("RealBinary = %q, want %q", got, realCodex)
	}
}

func TestRealBinaryConfinesDotPathEntry(t *testing.T) {
	sandbox(t)
	shimDir, _ := ShimBinDir()
	writeExe(t, filepath.Join(shimDir, "codex")) // a shim named codex, earlier on PATH
	cwd := t.TempDir()
	t.Chdir(cwd)
	realCodex := filepath.Join(cwd, "codex")
	writeExe(t, realCodex)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+".")

	got, err := RealBinary("codex")
	if err != nil {
		t.Fatal(err)
	}
	// LookPath may return a relative path for a "." entry; resolve against cwd to compare.
	abs, _ := filepath.Abs(got)
	if resolve(abs) == resolve(filepath.Join(shimDir, "codex")) {
		t.Fatalf("RealBinary resolved the shim via the '.' entry: %q", got)
	}
	if resolve(abs) != resolve(realCodex) {
		t.Fatalf("RealBinary = %q, want the cwd codex %q", got, realCodex)
	}
}

func TestRealBinarySkipsShimDirHoweverItIsSpelled(t *testing.T) {
	sandbox(t)
	shimDir, _ := ShimBinDir()
	realDir := t.TempDir()
	writeExe(t, filepath.Join(shimDir, "codex"))
	writeExe(t, filepath.Join(realDir, "codex"))
	link := filepath.Join(t.TempDir(), "shimlink")
	if err := os.Symlink(shimDir, link); err != nil {
		t.Fatal(err)
	}
	for _, spelled := range []string{shimDir + "/", link, shimDir + "/../bin"} {
		t.Setenv("PATH", spelled+string(os.PathListSeparator)+realDir)
		got, err := RealBinary("codex")
		if err != nil {
			t.Fatalf("%s: %v", spelled, err)
		}
		if resolve(got) != resolve(filepath.Join(realDir, "codex")) {
			t.Fatalf("PATH entry %q: RealBinary = %q, want the real binary", spelled, got)
		}
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

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func resolve(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}
