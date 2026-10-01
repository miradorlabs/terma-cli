package cli

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

// Claude Code ignores a repository that turns telemetry on, so the narrow mode, which
// leaves that to repositories, is refused before anything is written.
func TestConnectExportsReposIsRefusedForAnOffOnlyAgent(t *testing.T) {
	userSettings := userSandbox(t)
	out, err := runTerma(t, "connect", "claude", "--exports", "repos",
		"--api-key", testServerKey, "--team", testProjectID, "--yes")
	if err == nil || !strings.Contains(err.Error(), "ignores a repository that turns telemetry on") {
		t.Fatalf("narrow mode was accepted: %v\n%s", err, out)
	}
	if _, err := os.Stat(userSettings); err == nil {
		t.Fatalf("a refused connect wrote %s", userSettings)
	}
}

func TestConnectExportsRejectsUnknownValue(t *testing.T) {
	userSandbox(t)
	out, err := runTerma(t, "connect", "claude", "--exports", "weekly",
		"--api-key", testServerKey, "--team", testProjectID, "--yes")
	if err == nil {
		t.Fatalf("want an error:\n%s", out)
	}
	if !strings.Contains(err.Error(), "everywhere or repos") {
		t.Fatalf("the error should name the values: %v", err)
	}
}

// Claude Code ignores a repository that turns telemetry on, so a plain install writes none.
func TestInstallWritesNoTelemetryIntoTheRepository(t *testing.T) {
	repo := installRepo(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".claude", "settings.json")
	if settings := readClaudeSettings(t, path); len(settings) != 0 {
		t.Fatalf("plain install wrote telemetry settings: %+v", settings)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "terma hook") {
		t.Fatalf("the hooks are missing:\n%s", data)
	}
}

func TestUninstallRemovesRepoPolicy(t *testing.T) {
	repo := installRepo(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--yes", "--prompts", "off"); err != nil {
		t.Fatal(err)
	}
	if settings := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json")); settings["OTEL_LOG_USER_PROMPTS"] != "0" {
		t.Fatalf("policy not written: %+v", settings)
	}
	if _, err := runTerma(t, "uninstall", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); err == nil {
		settings := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json"))
		if _, ok := settings["OTEL_LOG_USER_PROMPTS"]; ok {
			t.Fatalf("uninstall left the repository policy behind: %+v", settings)
		}
	}
}

// With no journal, uninstall leaves an exporter value terma never writes (see renderedByTerma).
func TestUninstallKeepsAValueTermaNeverWrites(t *testing.T) {
	repo := installRepo(t)
	path := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env":{"OTEL_LOGS_EXPORTER":"console"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--yes"); err != nil {
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

// userSandbox also leaves the repository the test binary was built in, so status reads
// no real .terma/settings.json.
func userSandbox(t *testing.T) string {
	t.Helper()
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	fakeClaudeOnPath(t)
	return filepath.Join(claudeDir, "settings.json")
}

// fakeClaudeOnPath makes these tests independent of which agents the machine has
// installed: status and doctor report only a harness they can find.
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

// Re-installing a repository must not replace a team's narrower policy with defaults.
func TestInstallPreservesExistingRepositoryPolicy(t *testing.T) {
	for _, signals := range []string{"logs", "none"} {
		t.Run(signals, func(t *testing.T) {
			repo := installRepo(t)
			args := []string{"install", "--harness", "none", "--team", testProjectID, "--yes"}
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
			if got := readClaudeSettings(t, path)["OTEL_METRICS_EXPORTER"]; got == "none" {
				t.Fatalf("explicit policy update did not stop switching metrics off: %q", got)
			}
		})
	}
}

func TestInstallUpgradesHooksOnlyRepository(t *testing.T) {
	repo := installRepo(t)
	args := []string{"install", "--harness", "none", "--team", testProjectID, "--yes", "--prompts", "off"}
	if _, err := runTerma(t, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := local(t, claudeHarness(t).Harness, mustGetwd(t)).Disconnect(); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := readClaudeSettings(t, filepath.Join(repo, ".claude", "settings.json"))["OTEL_LOG_USER_PROMPTS"]; got != "0" {
		t.Fatalf("reinstall did not write the policy back: %q", got)
	}
	_, files, ok := strings.Cut(out, "Commit the new files")
	if !ok || !strings.Contains(files, ".claude/settings.json") {
		t.Fatalf("policy-only upgrade omitted commit instructions:\n%s", out)
	}
}

func TestInstallPreservesManuallyChangedRepositoryPolicy(t *testing.T) {
	repo := installRepo(t)
	args := []string{"install", "--harness", "none", "--team", testProjectID, "--yes"}
	if _, err := runTerma(t, append(args, "--signals", "logs", "--prompts", "off")...); err != nil {
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

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// A repository an earlier terma filled with on values is cleaned by the next install.
func TestInstallClearsAnEarlierTermasTelemetrySettings(t *testing.T) {
	repo := installRepo(t)
	path := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"env":{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":"1","OTEL_LOGS_EXPORTER":"otlp","OTEL_LOG_ASSISTANT_RESPONSES":"1","OTEL_LOG_TOOL_CONTENT":"1","OTEL_LOG_TOOL_DETAILS":"1","OTEL_LOG_USER_PROMPTS":"1","OTEL_METRICS_EXPORTER":"otlp","OTEL_TRACES_EXPORTER":"otlp"}}`
	if err := os.WriteFile(path, []byte(old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if settings := readClaudeSettings(t, path); len(settings) != 0 {
		t.Fatalf("the earlier settings survived: %+v", settings)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "terma hook") {
		t.Fatalf("the hooks are missing:\n%s", data)
	}
}
