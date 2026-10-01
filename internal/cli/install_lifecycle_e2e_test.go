//go:build unix

package cli

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

var (
	termaOnce sync.Once
	termaPath string
	termaErr  error
)

func termaBinary(t *testing.T) string {
	t.Helper()
	termaOnce.Do(func() {
		dir, err := os.MkdirTemp("", "terma-e2e-bin")
		if err != nil {
			termaErr = err
			return
		}
		termaPath = filepath.Join(dir, "terma")
		out, err := exec.Command("go", "build", "-o", termaPath, "github.com/miradorlabs/terma-cli/cmd/terma").CombinedOutput()
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

// The install lifecycle through the real binary, offline: install writes the binding and
// hooks, a re-run leaves the binding byte-identical, and uninstall removes it.
func TestE2E_InstallLifecycle(t *testing.T) {
	bin := termaBinary(t)
	cfgDir := t.TempDir()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "config", "user.email", "t@example.com"}, {"-C", repo, "config", "user.name", "t"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// One environment for every run, since it is part of the committed binding; dev, so
	// nothing reaches production.
	env := envWith("TERMA_CONFIG_DIR="+cfgDir, "TERMA_ENV=dev")
	binding := filepath.Join(repo, ".terma", "settings.json")

	runProc(t, bin, repo, env, "install", "--harness", "none", "--team", "proj-e2e-repo", "--adapters", "claude", "--yes")
	first, err := os.ReadFile(binding)
	if err != nil {
		t.Fatalf(".terma/settings.json not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".terma", "hooks", "post-commit")); err != nil {
		t.Fatalf("commit hook not written: %v", err)
	}

	// A colleague's re-run must not churn the committed binding.
	runProc(t, bin, repo, env, "install", "--harness", "none", "--team", "proj-e2e-repo", "--yes")
	second, _ := os.ReadFile(binding)
	if string(first) != string(second) {
		t.Fatalf("re-install churned the binding:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}

	// An already-bound repository re-installs without --project and without a sign-in;
	// --no-browser and the deadline guard against reaching auth.Login.
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
