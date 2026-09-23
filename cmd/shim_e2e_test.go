//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// These are end-to-end: they build the real terma binary and run `terma shim exec` as a
// subprocess (it replaces its process with execve, so it cannot be driven in-process),
// with fake `codex`/`claude` executables on PATH that print the environment they were
// handed. That is the one thing the unit tests cannot show — that an agent launched
// through the shim in a bound repository actually receives that repository's project.

var (
	termaOnce sync.Once
	termaPath string
	termaErr  error
)

// termaBinary builds the CLI once for the package and returns its path.
func termaBinary(t *testing.T) string {
	t.Helper()
	termaOnce.Do(func() {
		dir, err := os.MkdirTemp("", "terma-e2e-bin")
		if err != nil {
			termaErr = err
			return
		}
		termaPath = filepath.Join(dir, "terma")
		out, err := exec.Command("go", "build", "-o", termaPath, "github.com/miradorlabs/terma-cli").CombinedOutput()
		if err != nil {
			termaErr = fmt.Errorf("build terma: %w\n%s", err, out)
		}
	})
	if termaErr != nil {
		t.Fatal(termaErr)
	}
	return termaPath
}

const (
	e2eProjectID = "proj-e2e"
	e2eEndpoint  = "https://otel.example.com"
	e2eKey       = "ter_srv_e2e0123456789abcdef"
)

// e2eRouted sets up a sandbox config dir, a repo bound to e2eProjectID, its routing
// record and keys, and an original Codex home. It returns the config dir, the bound
// repo, and the original Codex home.
func e2eRouted(t *testing.T) (cfgDir, repo, codexHome string) {
	t.Helper()
	cfgDir = t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", cfgDir)

	repo = t.TempDir()
	if err := termaproject.Save(repo, &termaproject.File{Project: termaproject.Project{ID: e2eProjectID}}); err != nil {
		t.Fatal(err)
	}
	if err := shim.SaveRecord(shim.Record{
		ProjectID: e2eProjectID, Endpoint: e2eEndpoint,
		Signals: []string{"traces", "logs", "metrics"}, IncludePrompts: true, IncludeToolContent: true,
		Harnesses: []string{shim.AgentClaude, shim.AgentCodex},
	}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{shim.AgentClaude, shim.AgentCodex} {
		if err := keystore.SetFor(a, e2eProjectID, e2eKey); err != nil {
			t.Fatal(err)
		}
	}
	codexHome = t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	return cfgDir, repo, codexHome
}

// fakeAgents writes codex/claude scripts that echo what they were started with — the
// environment, and for claude its arguments too. It returns their directory.
func fakeAgents(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "codex"), `#!/bin/sh
echo "CODEX_HOME=$CODEX_HOME"
printf 'ARG=%s\n' "$@"
`)
	writeExecutable(t, filepath.Join(dir, "claude"), `#!/bin/sh
echo "ARGS=$*"
echo "ENV_HEADERS=$OTEL_EXPORTER_OTLP_HEADERS"
`)
	return dir
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// envWith returns the current environment with the given KEY=VALUE pairs overriding any
// existing entry for those keys.
func envWith(pairs ...string) []string {
	drop := map[string]bool{}
	for _, p := range pairs {
		if i := strings.IndexByte(p, '='); i >= 0 {
			drop[p[:i+1]] = true
		}
	}
	var out []string
	for _, e := range os.Environ() {
		keep := true
		for k := range drop {
			if strings.HasPrefix(e, k) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return append(out, pairs...)
}

// withPath returns the current environment with PATH replaced by the given directories.
func withPath(dirs ...string) []string {
	return envWith("PATH=" + strings.Join(dirs, string(os.PathListSeparator)))
}

func runProc(t *testing.T, bin, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(bin), args, err, out)
	}
	return string(out)
}

// A Codex session launched through `terma shim exec` in a bound repo is handed that
// project's runtime exporter; outside a bound repo it is a transparent pass-through.
func TestE2E_ShimExecRoutesCodex(t *testing.T) {
	bin := termaBinary(t)
	_, repo, codexHome := e2eRouted(t)
	env := withPath(fakeAgents(t))

	if out := runProc(t, bin, repo, env, "shim", "exec", "codex"); !strings.Contains(out, "CODEX_HOME="+codexHome) || !strings.Contains(out, e2eEndpoint) || !strings.Contains(out, "Bearer "+e2eKey) {
		t.Fatalf("codex in a bound repo must get the original CODEX_HOME and runtime exporter:\n%s", out)
	}
	// Outside any bound repo: no routing; original CODEX_HOME preserved.
	if out := runProc(t, bin, t.TempDir(), env, "shim", "exec", "codex"); strings.Contains(out, e2eEndpoint) || !strings.Contains(out, "CODEX_HOME="+codexHome) {
		t.Fatalf("codex outside a bound repo must pass through untouched:\n%s", out)
	}
	// -C chooses the destination binding, and user arguments survive unchanged.
	outside := t.TempDir()
	out := runProc(t, bin, outside, env, "shim", "exec", "codex", "--", "exec", "-C", repo, "hello world")
	if !strings.Contains(out, e2eEndpoint) || !strings.Contains(out, "ARG=hello world\n") || !strings.Contains(out, "ARG=-C\nARG="+repo+"\n") {
		t.Fatalf("-C did not route or preserve argv: %s", out)
	}
	out = runProc(t, bin, repo, env, "shim", "exec", "codex", "--", "-C", outside)
	if strings.Contains(out, e2eEndpoint) {
		t.Fatal("an unbound destination received telemetry overrides")
	}
	// Leaving CODEX_HOME unset preserves Codex's normal default-home selection.
	out = runProc(t, bin, repo, append(env, "CODEX_HOME="), "shim", "exec", "codex")
	if !strings.Contains(out, "CODEX_HOME=\n") {
		t.Fatal("routing injected a custom home")
	}

}

// A Claude session launched through `terma shim exec` in a bound repo is started with
// the project's settings document ahead of its own arguments. The document describes the
// export; the key reaches Claude Code through the headers helper it names, and never
// through the environment, which a machine-wide connect would outrank anyway.
func TestE2E_ShimExecRoutesClaude(t *testing.T) {
	bin := termaBinary(t)
	_, repo, _ := e2eRouted(t)
	env := withPath(fakeAgents(t))

	out := runProc(t, bin, repo, env, "shim", "exec", "claude", "--", "-p", "hello")
	args := strings.Fields(strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "ARGS="))
	if len(args) != 4 || args[0] != "--settings" || args[2] != "-p" || args[3] != "hello" {
		t.Fatalf("claude must be started with --settings <path> ahead of its own arguments:\n%s", out)
	}
	if !strings.Contains(out, "ENV_HEADERS=\n") {
		t.Fatalf("the key must not ride the agent's environment:\n%s", out)
	}
	// Read the document the way Claude Code would: the export from its env block, the
	// key from running the headers helper it names.
	data, err := os.ReadFile(args[1])
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env    map[string]string `json:"env"`
		Helper string            `json:"otelHeadersHelper"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("settings document: %v\n%s", err, data)
	}
	if doc.Env["OTEL_EXPORTER_OTLP_ENDPOINT"] != e2eEndpoint || doc.Env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Fatalf("the settings document must describe the project's export:\n%s", data)
	}
	if strings.Contains(string(data), e2eKey) {
		t.Fatalf("the key must live in the helper, not the document:\n%s", data)
	}
	headers, err := exec.Command(doc.Helper).Output()
	if err != nil {
		t.Fatalf("run the headers helper: %v", err)
	}
	if !strings.Contains(string(headers), "Bearer "+e2eKey) {
		t.Fatalf("the headers helper must hand Claude Code the project key, got %q", headers)
	}

	// Outside any bound repo: a transparent pass-through.
	if out := runProc(t, bin, t.TempDir(), env, "shim", "exec", "claude", "--", "-p", "hello"); !strings.Contains(out, "ARGS=-p hello\n") {
		t.Fatalf("claude outside a bound repo must pass through untouched:\n%s", out)
	}
}

// The PATH shim delivers the same routing: the shim script re-invokes terma, which finds
// the real binary past the shim directory and execs it with the project's environment.
func TestE2E_PathShimRoutes(t *testing.T) {
	bin := termaBinary(t)
	_, repo, codexHome := e2eRouted(t)
	agents := fakeAgents(t)

	shimBin, err := shim.InstallShims([]string{shim.AgentCodex, shim.AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	// The shim script calls `terma`, then terma finds the real codex past the shim dir:
	// so PATH is shim dir, then terma's dir, then the fake agents.
	env := withPath(shimBin, filepath.Dir(bin), agents)

	out := runProc(t, filepath.Join(shimBin, "codex"), repo, env)
	if !strings.Contains(out, "CODEX_HOME="+codexHome) || !strings.Contains(out, e2eEndpoint) || !strings.Contains(out, "Bearer "+e2eKey) {
		t.Fatalf("the PATH shim must route codex to the original CODEX_HOME and runtime exporter:\n%s", out)
	}
	out = runProc(t, filepath.Join(shimBin, "claude"), repo, env, "-p", "hello")
	if !strings.Contains(out, "ARGS=--settings ") || !strings.Contains(out, " -p hello\n") || !strings.Contains(out, "ENV_HEADERS=\n") {
		t.Fatalf("Claude PATH launcher did not deliver settings: %s", out)
	}

}

// The full install lifecycle through the real binary, offline (--harness none, a
// verbatim project id so no sign-in): install writes the committed binding + hooks,
// a re-run leaves the binding byte-identical, and uninstall removes it.
func TestE2E_InstallLifecycle(t *testing.T) {
	bin := termaBinary(t)
	cfgDir := t.TempDir()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "config", "user.email", "t@example.com"}, {"-C", repo, "config", "user.name", "t"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// One environment for every run: it is part of the committed binding, so a run under
	// another one would rewrite the file. dev, so that nothing here can reach production.
	env := envWith("TERMA_CONFIG_DIR="+cfgDir, "TERMA_ENV=dev")
	binding := filepath.Join(repo, ".terma", "settings.json")

	runProc(t, bin, repo, env, "install", "--harness", "none", "--project", "proj-e2e-repo", "--adapters", "claude", "--yes")
	first, err := os.ReadFile(binding)
	if err != nil {
		t.Fatalf(".terma/settings.json not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".terma", "hooks", "post-commit")); err != nil {
		t.Fatalf("commit hook not written: %v", err)
	}

	// A colleague's re-run must not churn the committed binding.
	runProc(t, bin, repo, env, "install", "--harness", "none", "--project", "proj-e2e-repo", "--yes")
	second, _ := os.ReadFile(binding)
	if string(first) != string(second) {
		t.Fatalf("re-install churned the binding:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}

	// Nor does an already-bound repository need a sign-in to be re-installed without
	// --project: there is no project to look up and no telemetry agent to mint a key for.
	// (--no-browser and the deadline are the guard rails: a regression here would reach
	// auth.Login, which otherwise opens a real browser and waits five minutes.)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rerun := exec.CommandContext(ctx, bin, "install", "--harness", "none", "--yes", "--no-browser", "--no-doctor")
	rerun.Dir, rerun.Env = repo, env
	if out, err := rerun.CombinedOutput(); err != nil {
		t.Fatalf("re-install of a bound repo must not need a sign-in: %v\n%s", err, out)
	}
	if third, _ := os.ReadFile(binding); string(first) != string(third) {
		t.Fatalf("re-install without --project churned the binding:\n%s", third)
	}

	runProc(t, bin, repo, env, "uninstall", "--yes")
	if _, err := os.Stat(binding); !os.IsNotExist(err) {
		t.Fatalf("binding survived uninstall: %v", err)
	}
}
