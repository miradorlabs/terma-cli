package cmd

import (
	"encoding/json"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

func TestParseReach(t *testing.T) {
	for raw, want := range map[string]harness.Reach{
		"":           harness.ReachEverywhere,
		"everywhere": harness.ReachEverywhere,
		"repos":      harness.ReachRepos,
		"  REPOS  ":  harness.ReachRepos,
	} {
		got, err := harness.ParseReach(raw)
		if err != nil || got != want {
			t.Fatalf("ParseReach(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := harness.ParseReach("sometimes"); err == nil {
		t.Fatal("want an error for an unknown value")
	}
}

// The narrow mode is the whole feature: the user file keeps everything a repository
// cannot hold — the destination, the credential and the master switch —
// and switches no exporter on. Without that split a repository policy has nothing to
// sit on and the arrangement silently sends nothing.
func TestConnectExportsReposLeavesExportersOffButStaysConnected(t *testing.T) {
	userSettings := userSandbox(t)
	out, err := runTerma(t, "connect", "claude", "--exports", "repos",
		"--api-key", testServerKey, "--project", testProjectID, "--identity", "dev@example.com", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	settings := readClaudeSettings(t, userSettings)

	for _, key := range []string{"OTEL_TRACES_EXPORTER", "OTEL_LOGS_EXPORTER", "OTEL_METRICS_EXPORTER"} {
		if settings[key] != "none" {
			t.Fatalf("%s = %q, want \"none\" — repositories are supposed to decide", key, settings[key])
		}
	}
	// The half that must survive, or a repository policy switches on an exporter with
	// nowhere to send and no key to send with.
	if settings["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Fatalf("the master switch must stay on: %q", settings["CLAUDE_CODE_ENABLE_TELEMETRY"])
	}
	if settings["OTEL_EXPORTER_OTLP_ENDPOINT"] == "" {
		t.Fatal("the endpoint must stay in the user file")
	}
	// The identity is Codex's and OpenCode's; Claude Code's settings never carry the
	// user's OTEL_RESOURCE_ATTRIBUTES, whatever --identity said.
	if v, ok := settings["OTEL_RESOURCE_ATTRIBUTES"]; ok {
		t.Fatalf("OTEL_RESOURCE_ATTRIBUTES=%q written into the user file", v)
	}

	// And it must read back as a deliberate arrangement, not as a broken connect.
	status, err := runTerma(t, "status")
	if err != nil {
		t.Fatalf("%v\n%s", err, status)
	}
	if !strings.Contains(status, "repositories decide what is sent") {
		t.Fatalf("status should explain the arrangement:\n%s", status)
	}
}

func TestConnectExportsRejectsUnknownValue(t *testing.T) {
	userSandbox(t)
	out, err := runTerma(t, "connect", "claude", "--exports", "weekly",
		"--api-key", testServerKey, "--project", testProjectID, "--yes")
	if err == nil {
		t.Fatalf("want an error:\n%s", out)
	}
	if !strings.Contains(err.Error(), "everywhere or repos") {
		t.Fatalf("the error should name the values: %v", err)
	}
}

// A repository policy on top of the narrow global connect is the arrangement working
// end to end: the repository turns exporters on, the machine holds everything else.
func TestRepoPolicyOverNarrowGlobalConnect(t *testing.T) {
	repo := installRepo(t)
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	fakeClaudeOnPath(t)
	userSettings := filepath.Join(claudeDir, "settings.json")
	if _, err := runTerma(t, "connect", "claude", "--exports", "repos",
		"--api-key", testServerKey, "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	project := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json"))
	if project["OTEL_TRACES_EXPORTER"] != "otlp" || project["OTEL_LOGS_EXPORTER"] != "otlp" {
		t.Fatalf("the repository should switch its signals on: %+v", project)
	}
	if project["OTEL_METRICS_EXPORTER"] != "otlp" {
		t.Fatalf("plain install should enable metrics too: %q", project["OTEL_METRICS_EXPORTER"])
	}
	// The committed file must never carry the parts that make it unsafe to commit.
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_RESOURCE_ATTRIBUTES"} {
		if _, ok := project[key]; ok {
			t.Fatalf("%s must never be written into a committed repository policy", key)
		}
	}
	if strings.Contains(strings.Join(valuesOf(project), " "), "ter_srv_") {
		t.Fatal("a credential reached the committed file")
	}
	// The user file is untouched by the repository's policy.
	user := readClaudeSettings(t, userSettings)
	if user["OTEL_TRACES_EXPORTER"] != "none" {
		t.Fatalf("the user file should still leave exporters off: %q", user["OTEL_TRACES_EXPORTER"])
	}
}

func TestInstallEnablesRepositoryTelemetryByDefault(t *testing.T) {
	repo := installRepo(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	settings := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json"))
	for _, key := range []string{"OTEL_TRACES_EXPORTER", "OTEL_LOGS_EXPORTER", "OTEL_METRICS_EXPORTER"} {
		if settings[key] != "otlp" {
			t.Fatalf("plain install must enable %s, got %q", key, settings[key])
		}
	}
}

func TestUninstallRemovesRepoPolicy(t *testing.T) {
	repo := installRepo(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if settings := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json")); settings["OTEL_TRACES_EXPORTER"] != "otlp" {
		t.Fatalf("policy not written: %+v", settings)
	}
	if _, err := runTerma(t, "uninstall", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); err == nil {
		settings := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json"))
		if _, ok := settings["OTEL_TRACES_EXPORTER"]; ok {
			t.Fatalf("uninstall left the repository policy behind: %+v", settings)
		}
	}
}

// userSandbox points Claude Code's config dir and Terma's at scratch directories and
// returns the user-level settings path. It also moves out of whatever repository the
// test binary was built in, so a status run here reads no real .terma/settings.json.
func userSandbox(t *testing.T) string {
	t.Helper()
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	fakeClaudeOnPath(t)
	return filepath.Join(claudeDir, "settings.json")
}

// fakeClaudeOnPath puts a `claude` executable on PATH.
//
// status and doctor only report a harness they can find, so without this these tests
// pass or fail according to whether the machine running them happens to have Claude
// Code installed — green on a developer's laptop, red on CI. The binary is never run
// for anything but --version.
func fakeClaudeOnPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs an executable shim on PATH")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho '2.1.0 (Claude Code)'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const testServerKey = "ter_srv_0123456789abcdef"

func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// readClaudeSettings returns the env block of a Claude Code settings file.
func readClaudeSettings(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	if doc.Env == nil {
		doc.Env = map[string]string{}
	}
	return doc.Env
}

// The one way this arrangement fails quietly: a developer narrows their machine, then
// works in a repository that never got a policy. Everything reads as connected and the
// repository sends nothing, so doctor has to be the thing that says it.
func TestDoctorFailsWhenThisRepositoryHasNoPolicy(t *testing.T) {
	repo := installRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeClaudeOnPath(t)
	if _, err := runTerma(t, "connect", "claude", "--exports", "repos",
		"--api-key", testServerKey, "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := (harness.Claude{}).Local(mustGetwd(t)).Disconnect(); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "FAIL  agent exporting to Terma") {
		t.Fatalf("doctor must fail the export check when no signals can be sent:\n%s", out)
	}
	if !strings.Contains(out, "only where a repository asks") {
		t.Fatalf("doctor should describe the arrangement:\n%s", out)
	}
	if !strings.Contains(out, "send nothing") {
		t.Fatalf("doctor should say this repository sends nothing:\n%s", out)
	}
	if !strings.Contains(out, "terma install") {
		t.Fatalf("doctor should say how to fix it:\n%s", out)
	}
	// status must tell the same story. It used to read "connected" and predict ~95%
	// coverage here — for a repository whose sessions send nothing.
	status, _ := runTerma(t, "status")
	if !strings.Contains(status, "sessions here send nothing") || strings.Contains(status, "~95%") {
		t.Fatalf("status disagrees with doctor about a repository that sends nothing:\n%s", status)
	}

	// With a policy the same repository is fine, and doctor says so rather than
	// staying quiet about an arrangement the reader may not remember choosing.
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _ = runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "this repository asks") {
		t.Fatalf("doctor should confirm the repository has a policy:\n%s", out)
	}
	if strings.Contains(out, "has no policy") {
		t.Fatalf("doctor still warns after the policy was written:\n%s", out)
	}
	status, _ = runTerma(t, "status")
	if strings.Contains(status, "send nothing") || !strings.Contains(status, "Claude Code → connected") {
		t.Fatalf("status should call a repository that asks connected:\n%s", status)
	}
	_ = repo
}

// Per-repo routing that is configured but not live (the shim is not on PATH) changes
// nothing about what a session sends — the machine-wide config still decides. So beside a
// connect that leaves it to repositories, such a repository sends nothing unless it asks,
// and both commands must say which: not "connected" from one and a warning from the other.
func TestStatusAndDoctorAgreeWhenRoutingIsConfiguredButNotLive(t *testing.T) {
	installRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(shim.WrapperEnv, "")
	fakeClaudeOnPath(t) // the real binary's directory only: terma's shim is not ahead of it
	if _, err := runTerma(t, "connect", "claude", "--exports", "repos",
		"--api-key", testServerKey, "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := (harness.Claude{}).Local(mustGetwd(t)).Disconnect(); err != nil {
		t.Fatal(err)
	}
	if err := shim.SaveRecord(shim.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai", Signals: []string{"logs"}, Harnesses: []string{shim.AgentClaude}}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(shim.AgentClaude, testProjectID, testServerKey); err != nil {
		t.Fatal(err)
	}

	status, _ := runTerma(t, "status")
	doc, _ := runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(status, "shim is not ahead of it") || strings.Contains(status, "~95%") {
		t.Fatalf("status should say the routing is not live, and not count it:\n%s", status)
	}
	if !strings.Contains(doc, "not ahead of the agent on your PATH") {
		t.Fatalf("doctor should say the routing is not live:\n%s", doc)
	}

	// The repository asks: sessions send through the machine-wide config, to this project.
	// That works, and neither command may claim otherwise because the shim is missing.
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	status, _ = runTerma(t, "status")
	doc, _ = runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(status, "Claude Code → connected") || strings.Contains(status, "not ahead") {
		t.Fatalf("status should call it connected:\n%s", status)
	}
	if strings.Contains(doc, "not ahead of the agent") || strings.Contains(doc, "send nothing") {
		t.Fatalf("doctor should not warn about a repository whose sessions do send:\n%s", doc)
	}
}

// Re-installing a repository must not replace a team's narrower policy with defaults.
func TestInstallPreservesExistingRepositoryPolicy(t *testing.T) {
	for _, signals := range []string{"logs", "none"} {
		t.Run(signals, func(t *testing.T) {
			repo := installRepo(t)
			args := []string{"install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor"}
			if out, err := runTerma(t, append(args, "--signals", signals, "--exclude-prompts", "--exclude-tool-content")...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			path := filepath.Join(repo, ".claude", "settings.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := runTerma(t, args...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("plain reinstall changed the existing telemetry policy")
			}
			if out, err := runTerma(t, append(args, "--signals", "metrics")...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := readClaudeSettings(t, path)["OTEL_METRICS_EXPORTER"]; got != "otlp" {
				t.Fatalf("explicit policy update did not enable metrics: %q", got)
			}
		})
	}
}

func TestInstallUpgradesHooksOnlyRepository(t *testing.T) {
	repo := installRepo(t)
	args := []string{"install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor"}
	if _, err := runTerma(t, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := (harness.Claude{}).Local(mustGetwd(t)).Disconnect(); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json"))["OTEL_LOGS_EXPORTER"]; got != "otlp" {
		t.Fatalf("reinstall did not enable telemetry: %q", got)
	}
	_, files, ok := strings.Cut(out, "Commit these files")
	if !ok || !strings.Contains(files, ".claude/settings.json") {
		t.Fatalf("policy-only upgrade omitted commit instructions:\n%s", out)
	}
}

func TestInstallPreservesManuallyChangedRepositoryPolicy(t *testing.T) {
	repo := installRepo(t)
	args := []string{"install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor"}
	if _, err := runTerma(t, args...); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	env := doc["env"].(map[string]any)
	// Change every policy value so none still match the connect journal.
	for key := range env {
		if strings.HasSuffix(key, "_EXPORTER") {
			env[key] = "none"
		} else {
			env[key] = "0"
		}
	}
	before, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, args...); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("reinstall replaced a manually disabled policy")
	}
}
