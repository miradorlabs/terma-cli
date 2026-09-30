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
)

// End-to-end through the real terma binary, built once, run as a subprocess.

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
	// Never the real service manager: an install would register this sandbox's relay.
	return append(append(out, "TERMA_RELAY_SERVICE=0"), pairs...)
}

// withPath returns the current environment with PATH replaced by the given directories.
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
