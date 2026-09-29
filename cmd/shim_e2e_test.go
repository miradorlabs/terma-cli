//go:build unix

package cmd

import (
	"context"
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
	// Model a fresh launch even when the test runner is itself inside routed Codex.
	t.Setenv(shim.CodexRoutedEnv, "")

	repo = t.TempDir()
	if err := termaproject.Save(repo, &termaproject.File{Project: termaproject.Project{ID: e2eProjectID}}); err != nil {
		t.Fatal(err)
	}
	if err := shim.SaveRecord(shim.Record{
		ProjectID: e2eProjectID, Endpoint: e2eEndpoint,
		Signals: []string{"traces", "logs", "metrics"}, IncludePrompts: true, IncludeToolContent: true,
		Harnesses: []string{shim.AgentClaude, shim.AgentCodex}, CLI: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{shim.AgentClaude, shim.AgentCodex} {
		if err := keystore.SetFor(a, e2eProjectID, e2eKey, keystore.Hosts{}); err != nil {
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
echo "TERMA_CODEX_ROUTED=$TERMA_CODEX_ROUTED"
printf 'ARG=%s\n' "$@"
`)
	writeExecutable(t, filepath.Join(dir, "claude"), `#!/bin/sh
echo "ARGS=$*"
echo "ENV_HEADERS=$OTEL_EXPORTER_OTLP_HEADERS"
`)
	return dir
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

// terma no longer routes an agent per repository, but a PATH shim an earlier terma
// installed may still front one until the next setup, install or refresh removes it. The
// shim's entry points must start the real agent exactly as it was invoked, in a bound
// repository or anywhere else.
func TestE2E_ShimExecPassesThrough(t *testing.T) {
	bin := termaBinary(t)
	_, repo, codexHome := e2eRouted(t)
	env := withPath(fakeAgents(t))
	for _, dir := range []string{repo, t.TempDir()} {
		out := runProc(t, bin, dir, env, "shim", "exec", "codex", "--", "exec", "hello world")
		if !strings.Contains(out, "CODEX_HOME="+codexHome) || !strings.Contains(out, "TERMA_CODEX_ROUTED=\n") ||
			strings.Contains(out, e2eEndpoint) || !strings.Contains(out, "ARG=exec\nARG=hello world\n") || strings.Count(out, "ARG=") != 2 {
			t.Fatalf("codex must start unchanged in %s:\n%s", dir, out)
		}
		if out := runProc(t, bin, dir, env, "shim", "exec", "claude", "--", "-p", "hello"); !strings.Contains(out, "ARGS=-p hello\n") {
			t.Fatalf("claude must start unchanged in %s:\n%s", dir, out)
		}
	}
}

// The launcher protocol an old PATH shim speaks: it asks `terma shim prepare` for the
// arguments to put ahead of the agent's own and reads them back. The answer is none.
func TestE2E_ShimPrepareAnswersNothingToAdd(t *testing.T) {
	bin := termaBinary(t)
	_, repo, _ := e2eRouted(t)
	plan := t.TempDir()
	runProc(t, bin, repo, withPath(fakeAgents(t)), "shim", "prepare", "codex", plan, "--", "exec", "hi")
	entries, err := os.ReadDir(plan)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(plan, "count"))
	if len(entries) != 1 || string(data) != "terma-args-v1:0\n" {
		t.Fatalf("prepare wrote %d file(s), count %q", len(entries), data)
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
