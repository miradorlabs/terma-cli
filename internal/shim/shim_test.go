package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

const (
	testProjectID = "770e8400-e29b-41d4-a716-446655440000"
	testKey       = "ter_srv_0123456789abcdef"
	testEndpoint  = "https://otel-dev.example.com"
)

// sandbox points config.Dir() and the home directory at temporary directories so a test
// never reads or writes real state.
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

func boundRepo(t *testing.T, pid string) string {
	t.Helper()
	repo := t.TempDir()
	if err := termaproject.Save(repo, &termaproject.File{Project: termaproject.Project{ID: pid}}); err != nil {
		t.Fatal(err)
	}
	return repo
}

// claudeRouted prepares a bound repo whose project routes Claude Code.
func claudeRouted(t *testing.T) string {
	t.Helper()
	repo := boundRepo(t, testProjectID)
	if err := SaveRecord(Record{ProjectID: testProjectID, Endpoint: testEndpoint, Signals: []string{"traces", "logs", "metrics"}, Harnesses: []string{AgentClaude}}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(AgentClaude, testProjectID, testKey); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Claude Code is routed on its command line, not through the environment: a developer's
// machine-wide connect (an env block and a headers helper in ~/.claude/settings.json)
// outranks the environment, and only --settings outranks that.
func TestRouteClaudeHandsOverASettingsDocument(t *testing.T) {
	sandbox(t)
	repo := claudeRouted(t)
	r := routeFor(AgentClaude, repo, []string{"-p", "hi"})
	if len(r.env) != 0 {
		t.Fatalf("the key must not ride the agent's environment: %v", r.env)
	}
	if len(r.args) != 2 || r.args[0] != "--settings" {
		t.Fatalf("args = %v, want --settings <path>", r.args)
	}
	var doc struct {
		Env    map[string]string `json:"env"`
		Helper string            `json:"otelHeadersHelper"`
	}
	data, err := os.ReadFile(r.args[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("settings document: %v\n%s", err, data)
	}
	if doc.Env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" || doc.Env["OTEL_EXPORTER_OTLP_ENDPOINT"] != testEndpoint {
		t.Fatalf("export not described: %+v", doc.Env)
	}
	if strings.Contains(string(data), testKey) {
		t.Fatalf("the key landed in the settings document:\n%s", data)
	}
	script, err := os.ReadFile(doc.Helper)
	if err != nil {
		t.Fatalf("headers helper: %v", err)
	}
	if !strings.Contains(string(script), testKey) {
		t.Fatal("the headers helper does not hold the key")
	}
	if fi, _ := os.Stat(doc.Helper); fi.Mode().Perm() != 0o700 {
		t.Fatalf("helper mode = %v, want 0700", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(r.args[1]); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v, want 0600", fi.Mode().Perm())
	}
}

// Explicit user settings are passed through without partial telemetry overrides.
func TestRouteClaudeYieldsToTheDevelopersOwnSettingsFlag(t *testing.T) {
	sandbox(t)
	repo := claudeRouted(t)
	for _, args := range [][]string{{"--settings", "mine.json"}, {"--settings=mine.json", "-p", "hi"}} {
		r := routeFor(AgentClaude, repo, args)
		if len(r.args) != 0 {
			t.Fatalf("%v: terma added %v beside the developer's --settings", args, r.args)
		}
		if len(r.env) != 0 {
			t.Fatalf("%v: unexpected environment override: %v", args, r.env)
		}
	}
	// After "--" it is a prompt, not a flag.
	if r := routeFor(AgentClaude, repo, []string{"--", "--settings"}); len(r.args) != 2 {
		t.Fatalf("a literal after -- must not be read as the flag: %+v", r)
	}
}

func TestRouteCodexPreservesHome(t *testing.T) {
	sandbox(t)
	repo := boundRepo(t, testProjectID)
	if err := SaveRecord(Record{ProjectID: testProjectID, Endpoint: testEndpoint, Signals: []string{"logs"}, Harnesses: []string{AgentCodex}, CLI: true}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(AgentCodex, testProjectID, testKey); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	config := "model = \"custom\"\nnotify = [\"my-notifier\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	r := routeFor(AgentCodex, repo, nil)
	if r.env[CodexRoutedEnv] != "1" || os.Getenv("CODEX_HOME") != home {
		t.Fatal("routing did not mark Codex without changing its home")
	}
	args := strings.Join(r.args, " ")
	if !strings.Contains(args, testEndpoint) || !strings.Contains(args, "Bearer "+testKey) {
		t.Fatal("missing runtime exporter")
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || string(data) != config {
		t.Fatal("routing modified user configuration")
	}
	if strings.Contains(args, "notify") {
		t.Fatal("routing replaced notifier")
	}
}

func TestDesktopOnlyRouteDoesNotConfigureCodexCLI(t *testing.T) {
	sandbox(t)
	repo := boundRepo(t, testProjectID)
	cli, desktop := false, true
	if err := SaveRecord(Record{ProjectID: testProjectID, Endpoint: testEndpoint, Signals: []string{"logs"},
		Harnesses: []string{AgentCodex}, CLI: cli, Desktop: desktop}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(AgentCodex, testProjectID, testKey); err != nil {
		t.Fatal(err)
	}
	if r := routeFor(AgentCodex, repo, nil); len(r.args) != 0 || len(r.env) != 0 {
		t.Fatalf("desktop-only project changed a CLI launch: %+v", r)
	}
}

func TestCodexWorkingDir(t *testing.T) {
	cwd := t.TempDir()
	abs := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default", nil, cwd},
		{"short", []string{"-C", "../personal"}, filepath.Join(cwd, "../personal")},
		{"long", []string{"--cd", abs}, abs},
		{"equals", []string{"--cd=" + abs}, abs},
		{"attached", []string{"-C" + abs}, abs},
		{"short equals", []string{"-C=" + abs}, abs},
		{"exec", []string{"exec", "--cd", abs, "hello"}, abs},
		{"resume", []string{"resume", "--last", "-C", abs}, abs},
		{"after prompt", []string{"hello", "-C", abs}, abs},
		{"separator", []string{"--", "--cd", abs}, cwd},
		{"option value", []string{"--title", "--cd", abs}, cwd},
		{"missing value", []string{"-C"}, cwd},
		{"last override", []string{"-C", abs, "exec", "-C", "child"}, filepath.Join(cwd, "child")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexWorkingDir(cwd, tc.args); got != tc.want {
				t.Fatalf("working dir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexRouteUsesDestinationBinding(t *testing.T) {
	sandbox(t)
	work := boundRepo(t, testProjectID)
	personalID := "880e8400-e29b-41d4-a716-446655440000"
	personal := boundRepo(t, personalID)
	unbound := t.TempDir()
	for _, id := range []string{testProjectID, personalID} {
		if err := SaveRecord(Record{ProjectID: id, Signals: []string{"logs"}, Harnesses: []string{AgentCodex}, CLI: true}); err != nil {
			t.Fatal(err)
		}
		if err := keystore.SetFor(AgentCodex, id, testKey+id); err != nil {
			t.Fatal(err)
		}

	}
	for _, tc := range []struct{ from, to, project string }{
		{work, personal, personalID},
		{personal, work, testProjectID},
		{work, unbound, ""},
		{unbound, work, testProjectID},
	} {
		rel, err := filepath.Rel(tc.from, tc.to)
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"-C", tc.to}, {"exec", "--cd=" + rel}} {
			want := ""
			if tc.project != "" {
				want = testKey + tc.project
			}
			got := strings.Join(routeFor(AgentCodex, tc.from, args).args, " ")
			if (want == "" && got != "") || (want != "" && !strings.Contains(got, want)) {
				t.Fatalf("from %s, args %v: runtime args = %q, want %q", tc.from, args, got, want)
			}
		}
	}
}

func TestRouteEnvPassesThroughWhenNotApplicable(t *testing.T) {
	sandbox(t)
	// No binding at all.
	routes := func(agent, cwd string) bool {
		r := routeFor(agent, cwd, nil)
		return len(r.env) > 0 || len(r.args) > 0
	}
	if routes(AgentClaude, t.TempDir()) {
		t.Fatal("unbound repo should not route")
	}
	// Bound, but no record.
	repo := boundRepo(t, testProjectID)
	if routes(AgentClaude, repo) {
		t.Fatal("no record should not route")
	}
	// Record exists but does not list the agent.
	if err := SaveRecord(Record{ProjectID: testProjectID, Endpoint: testEndpoint, Harnesses: []string{AgentCodex}, CLI: true}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(AgentClaude, testProjectID, testKey); err != nil {
		t.Fatal(err)
	}
	if routes(AgentClaude, repo) {
		t.Fatal("agent absent from record should not route")
	}
	// A non-routable agent never routes.
	if routes("cursor", repo) {
		t.Fatal("cursor is not routable")
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

func TestInstallAndRemoveShims(t *testing.T) {
	sandbox(t)
	binDir, err := InstallShims([]string{AgentCodex, AgentClaude, "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(binDir, "codex"))
	if err != nil {
		t.Fatalf("codex shim not written: %v", err)
	}
	if !strings.Contains(string(data), "terma shim prepare") {
		t.Fatalf("shim does not call terma: %s", data)
	}
	// A non-routable agent gets no shim.
	if _, err := os.Stat(filepath.Join(binDir, "cursor")); !os.IsNotExist(err) {
		t.Fatalf("cursor should not be shimmed, stat err = %v", err)
	}
	if err := RemoveShims(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(binDir, "codex")); !os.IsNotExist(err) {
		t.Fatalf("shim survived removal: %v", err)
	}
}

func TestRemoveAllTearsDownRoutingState(t *testing.T) {
	sandbox(t)
	if _, err := InstallShims([]string{AgentCodex}); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecord(Record{ProjectID: testProjectID, Endpoint: testEndpoint, Harnesses: []string{AgentCodex}, CLI: true}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(); err != nil {
		t.Fatal(err)
	}
	binDir, _ := ShimBinDir()
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("shim dir survived: %v", err)
	}
	if _, ok, _ := LoadRecord(testProjectID); ok {
		t.Fatal("routing record survived RemoveAll")
	}
	// RemoveAll on a clean machine is not an error.
	if err := RemoveAll(); err != nil {
		t.Fatalf("RemoveAll twice: %v", err)
	}
}

func TestWrapperSnippetRoutesRoutableAgentsOnly(t *testing.T) {
	snip := WrapperSnippet([]string{AgentCodex, "cursor", AgentClaude})
	if !strings.Contains(snip, "codex() { if [ -x") {
		t.Fatalf("missing codex wrapper:\n%s", snip)
	}
	if strings.Contains(snip, "cursor()") {
		t.Fatalf("cursor is not routable and must not be wrapped:\n%s", snip)
	}
}

// A "." on PATH must be searched as the current directory, not collapsed to a bare name
// that exec.LookPath would resolve against the whole PATH — which could re-include the
// shim directory and make the shim resolve itself.
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

// Live means the agent's name resolves to the shim: on PATH but behind the real binary
// routes nothing, and must not be reported as live.
func TestActiveNeedsTheShimAheadOfTheRealBinary(t *testing.T) {
	sandbox(t)
	t.Setenv(WrapperEnv, "")
	binDir, err := InstallShims([]string{AgentCodex})
	if err != nil {
		t.Fatal(err)
	}
	realDir := t.TempDir()
	writeExe(t, filepath.Join(realDir, "codex"))
	sep := string(os.PathListSeparator)

	t.Setenv("PATH", realDir+sep+binDir)
	if Active(AgentCodex) {
		t.Fatal("shim behind the real binary must not count as live")
	}
	t.Setenv("PATH", binDir+sep+realDir)
	if !Active(AgentCodex) {
		t.Fatal("shim ahead of the real binary is live")
	}
	t.Setenv("PATH", realDir)
	if Active(AgentCodex) {
		t.Fatal("shim off PATH is not live")
	}
	// Removed → not live, even with its directory still first on PATH.
	t.Setenv("PATH", binDir+sep+realDir)
	if err := RemoveShims(); err != nil {
		t.Fatal(err)
	}
	if Active(AgentCodex) {
		t.Fatal("a removed shim is not live")
	}
	// The shell wrapper cannot be seen from a child process; its marker can.
	t.Setenv(WrapperEnv, "wrapper")
	if !Active(AgentCodex) {
		t.Fatal("a loaded wrapper is live")
	}
}

// With neither terma nor the agent to name, the fallback must not be a bare name: from
// the shim directory that resolves to the script itself.
func TestShimFallbackNeverExecsItself(t *testing.T) {
	sandbox(t)
	t.Setenv("PATH", t.TempDir()) // the agent is not installed
	binDir, err := InstallShims([]string{AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(binDir, AgentClaude))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "exit 127") {
		t.Fatalf("fallback does not fail cleanly:\n%s", data)
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

func TestClaudeRouteRefreshesKeysAndMasksInheritedDestinations(t *testing.T) {
	sandbox(t)
	repo := claudeRouted(t)
	first := routeFor(AgentClaude, repo, nil)
	if len(first.args) != 2 {
		t.Fatal("missing initial route")
	}
	if err := keystore.SetFor(AgentClaude, testProjectID, "ter_srv_fedcba9876543210"); err != nil {
		t.Fatal(err)
	}
	routeFor(AgentClaude, repo, nil)
	data, err := os.ReadFile(first.args[1])
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env    map[string]string `json:"env"`
		Helper string            `json:"otelHeadersHelper"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, signal := range []string{"LOGS", "METRICS", "TRACES"} {
		prefix := "OTEL_EXPORTER_OTLP_" + signal
		if doc.Env[prefix+"_ENDPOINT"] != testEndpoint+"/v1/"+strings.ToLower(signal) || doc.Env[prefix+"_PROTOCOL"] != "http/protobuf" || doc.Env[prefix+"_HEADERS"] != "" {
			t.Fatalf("unmasked %s destination", signal)
		}
	}
	if doc.Env["ENABLE_BETA_TRACING_DETAILED"] != "0" || doc.Env["BETA_TRACING_ENDPOINT"] != "" {
		t.Fatal("beta destination not masked")
	}
	helper, err := os.ReadFile(doc.Helper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(helper), "ter_srv_fedcba9876543210") || strings.Contains(string(helper), testKey) {
		t.Fatal("stale credential")
	}
	// A damaged settings path must not switch to a partial environment route.
	if err := os.Remove(first.args[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(first.args[1], 0700); err != nil {
		t.Fatal(err)
	}
	r := routeFor(AgentClaude, repo, nil)
	if len(r.args) != 0 || len(r.env) != 0 {
		t.Fatal("failed preparation did not pass through")
	}
}

func TestCodexRouteRequiresExplicitCLIChoice(t *testing.T) {
	sandbox(t)
	repo := boundRepo(t, testProjectID)
	if err := SaveRecord(Record{ProjectID: testProjectID, Endpoint: testEndpoint, Harnesses: []string{AgentCodex}, Signals: []string{"logs"}}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(AgentCodex, testProjectID, testKey); err != nil {
		t.Fatal(err)
	}
	if got := routeFor(AgentCodex, repo, nil); len(got.args) != 0 || len(got.env) != 0 {
		t.Fatalf("Codex routed without a CLI choice: %+v", got)
	}
}

// A refresh rewrites only terma's own shims that differ from this build's script: an
// absent shim stays absent, and a file terma did not write is left alone.
func TestRefreshShimsRewritesOnlyInstalledStaleShims(t *testing.T) {
	sandbox(t)
	if changed, err := RefreshShims(); err != nil || len(changed) != 0 {
		t.Fatalf("no shims: changed=%v err=%v", changed, err)
	}
	binDir, err := InstallShims([]string{AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(binDir, AgentClaude)
	want, _ := os.ReadFile(claude)
	if err := os.WriteFile(claude, []byte(shimHeader(AgentClaude)+" An earlier build.\nexec terma shim exec claude \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := []byte("#!/bin/sh\nexec /opt/codex \"$@\"\n")
	if err := os.WriteFile(filepath.Join(binDir, AgentCodex), theirs, 0o755); err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshShims()
	if err != nil || len(changed) != 1 || changed[0] != claude {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(claude); string(got) != string(want) {
		t.Fatalf("claude shim not refreshed:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(binDir, AgentCodex)); string(got) != string(theirs) {
		t.Fatalf("a file terma did not write was rewritten:\n%s", got)
	}
	if changed, err := RefreshShims(); err != nil || len(changed) != 0 {
		t.Fatalf("second refresh: changed=%v err=%v", changed, err)
	}
}

// A record written by 0.0.2 has no cli field, and today's router reads that as "do not
// route the Codex CLI". The migration restores what the record meant, once, and touches
// nothing else.
func TestMigrateCodexCLIRoutesFillsInWhatOldRecordsMeant(t *testing.T) {
	sandbox(t)
	if err := MigrateCodexCLIRoutes(); err != nil {
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

	if err := MigrateCodexCLIRoutes(); err != nil {
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
	before, _ := os.ReadFile(old)
	if err := MigrateCodexCLIRoutes(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(old); string(after) != string(before) {
		t.Fatal("a second run changed a migrated record")
	}
}
