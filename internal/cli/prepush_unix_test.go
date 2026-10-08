//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/repohooks"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The pre-push hook terma installs, run by a real git push, reports the push once git
// has exited, from a detached terma, without holding the push up.
func TestARealPushIsReportedByTheInstalledHook(t *testing.T) {
	bin := termaBinary(t)
	configDir, stateDir := t.TempDir(), t.TempDir()
	useDirs(t, configDir, stateDir)
	t.Setenv("TERMA_CONFIG_DIR", configDir)
	t.Setenv("TERMA_STATE_DIR", stateDir)
	t.Setenv("TERMA_HOOKS", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))

	root, _ := filepath.EvalSymlinks(t.TempDir())
	remote := filepath.Join(t.TempDir(), "backup.git")
	gitOut(t, root, "init", "-q", "-b", "main")
	gitOut(t, root, "config", "user.email", "dev@example.com")
	gitOut(t, root, "config", "user.name", "Dev")
	gitOut(t, root, "remote", "add", "origin", "git@github.com:acme/"+filepath.Base(root)+".git")
	gitOut(t, root, "init", "-q", "--bare", "-b", "main", remote)
	gitOut(t, root, "remote", "add", "backup", remote)
	gitOut(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	gitOut(t, root, "push", "-q", "backup", "main")
	admitHere(t, root, "team")
	hookruntest.RelayOn(t, stateDir)
	if _, err := repohooks.Install(stateDir, bin, filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}

	gitOut(t, root, "checkout", "-q", "-b", "feature")
	gitOut(t, root, "commit", "-q", "--allow-empty", "-m", "agent work\n\nAgent-Session-Id: sess-push\nAgent-Tool: claude")
	head := gitOut(t, root, "rev-parse", "HEAD")
	start := time.Now()
	gitOut(t, root, "push", "-q", "backup", "feature")
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the push took %s", took)
	}

	events := filepath.Join(stateDir, spool.Dir, "events.jsonl")
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, _ := os.ReadFile(events)
		if strings.Contains(string(data), `"`+semconv.TermaPushEvent+`"`) {
			for _, want := range []string{head, `"sess-push"`, semconv.TermaPushStatusTrackingRefUpdated, `"refs/heads/feature"`} {
				if !strings.Contains(string(data), want) {
					t.Errorf("the push event lacks %s:\n%s", want, data)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no push event after 30s; spool:\n%s", data)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The record is gone once reported.
	deadline = time.Now().Add(10 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(stateDir, "pushes"))
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("push records left: %v", entries)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
