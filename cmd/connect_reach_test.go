package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
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

func TestUninstallRemovesRepoPolicy(t *testing.T) {
	repo := installRepo(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor"); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "connect", "claude", "--scope", "local", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
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

// A repository whose team set Claude Code's exporter to a value terma never writes: with
// no record of writing it, uninstall takes out only terma's hooks and leaves the setting.
// (A value terma does write is removable from any clone: see renderedByTerma.)
func TestUninstallKeepsAValueTermaNeverWrites(t *testing.T) {
	repo := installRepo(t)
	path := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env":{"OTEL_LOGS_EXPORTER":"console"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor"); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "uninstall", "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if settings := readClaudeSettings(t, path); settings["OTEL_LOGS_EXPORTER"] != "console" {
		t.Fatalf("uninstall removed the team's own exporter setting: %+v", settings)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "terma hook") {
		t.Fatalf("uninstall left terma's hooks behind:\n%s", data)
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

// fakeClaudeOnPath puts only a fake Claude and git on PATH, excluding other installed
// agents so their real configuration cannot affect these Claude-specific checks.
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
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

const testServerKey = "ter_srv_0123456789abcdef"

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
	if !strings.Contains(out, "terma setup --signals") {
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
	if out, err := runTerma(t, "connect", "claude", "--scope", "local", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
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

// A repository policy this machine wrote and somebody has since changed is theirs:
// install takes out only what still holds the value terma wrote.
func TestInstallPreservesManuallyChangedRepositoryPolicy(t *testing.T) {
	repo := installRepo(t)
	args := []string{"install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor"}
	if out, err := runTerma(t, args...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := runTerma(t, "connect", "claude", "--scope", "local", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
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

// A telemetry policy an earlier `terma install` committed outranks the machine-wide
// configuration in its repository — an exporter left at none would switch the machine's
// export off there — so install takes it out, including in a clone with no record of
// writing it, and lists the file to commit.
func TestInstallStripsAnEarlierRepositoryPolicy(t *testing.T) {
	repo := installRepo(t)
	path := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := `{"env":{"OTEL_TRACES_EXPORTER":"none","OTEL_LOGS_EXPORTER":"otlp","OTEL_METRICS_EXPORTER":"otlp","OTEL_LOG_USER_PROMPTS":"1","OTEL_LOGS_EXPORT_INTERVAL":"5000"}}`
	if err := os.WriteFile(path, []byte(policy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	settings := readClaudeSettings(t, path)
	for _, key := range []string{"OTEL_TRACES_EXPORTER", "OTEL_LOGS_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOG_USER_PROMPTS"} {
		if _, ok := settings[key]; ok {
			t.Fatalf("install left %s in the repository policy: %+v", key, settings)
		}
	}
	if settings["OTEL_LOGS_EXPORT_INTERVAL"] != "5000" {
		t.Fatalf("install removed a setting terma never writes: %+v", settings)
	}
	if _, files, ok := strings.Cut(out, "Commit these files"); !ok || !strings.Contains(files, ".claude/settings.json") {
		t.Fatalf("the stripped policy is not listed to commit:\n%s", out)
	}
}
