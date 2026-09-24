package cmd

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// gitRepoHere makes the current directory a git repository (authSandbox already moved
// the test into a scratch one).
func gitRepoHere(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q", "."}, {"config", "user.email", "dev@example.com"}, {"config", "user.name", "Dev"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A signed-in developer wiring only committed hooks still needs a delivery key,
// even without selecting a telemetry harness. Install must mint it once and reuse it.
func TestInstallGivesAHooksOnlyDeveloperAKeyToDeliverWith(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	gitRepoHere(t)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	const project = "aaaaaaaa-0000-4000-8000-000000000001"

	out, err := within(20*time.Second).combined(t, "install", "--harness", "none", "--adapters", "cursor", "--project", project, "--yes", "--no-doctor", "--no-browser")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if keystore.Get(project) == "" || f.keysMint.Load() != 1 {
		t.Fatalf("no key to deliver hook events with (mints=%d):\n%s", f.keysMint.Load(), out)
	}
	if !strings.Contains(out, "Project key stored") {
		t.Fatalf("install should say it stored a key:\n%s", out)
	}

	// Once is enough: a re-install reuses the key instead of leaving a trail of them.
	if out, err := within(20*time.Second).combined(t, "install", "--harness", "none", "--adapters", "cursor", "--yes", "--no-doctor", "--no-browser"); err != nil {
		t.Fatalf("re-install: %v\n%s", err, out)
	}
	if f.keysMint.Load() != 1 {
		t.Fatalf("re-install minted again: %d keys", f.keysMint.Load())
	}
}

// Wiring a repository with no agent of one's own must stay possible without signing in —
// CI does it, and so does whoever onboards a repository. It says what is missing instead
// of pretending: the hooks are in, and their events wait for a key.
func TestInstallWithoutACredentialSaysItsEventsAreHeld(t *testing.T) {
	installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "held until it has a key") || !strings.Contains(out, "terma setup") {
		t.Fatalf("install should say hook events are held, and how to fix it:\n%s", out)
	}
	if keystore.Get(testProjectID) != "" {
		t.Fatal("a key appeared from nowhere")
	}
}

// A repository with no hooks produces no events, so there is nothing to hold and nothing
// to say. (One whose hooks are already in is a different matter, even under --no-hooks.)
func TestInstallWithoutHooksSaysNothingAboutAKey(t *testing.T) {
	installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--no-hooks", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if strings.Contains(out, "held until") {
		t.Fatalf("no hooks, no events to hold:\n%s", out)
	}
}

func TestInstallNeedsAuth(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const id = "770e8400-e29b-41d4-a716-446655440000"
	bound := &termaproject.File{Project: termaproject.Project{ID: id}}
	for _, c := range []struct {
		name     string
		agents   []string
		ref      string
		existing *termaproject.File
		hooks    bool
		want     bool
	}{
		{"a telemetry agent mints a key", []string{"claude"}, id, nil, true, true},
		{"Codex desktop mints a Codex key", []string{codexDesktopAgent}, id, nil, true, true},
		{"a project name needs a lookup", nil, "Acme Web", nil, true, true},
		{"no binding and no --project is a picker", nil, "", nil, true, true},
		{"wiring a repository with no agent of one's own", nil, id, nil, true, false},
		{"…and re-wiring a bound one", nil, "", bound, true, false},
		{"a hooks-only agent needs a key to deliver with", []string{"cursor"}, "", bound, true, true},
		{"…unless it installs no hooks", []string{"cursor"}, "", bound, false, false},
	} {
		if got := installNeedsAuth(c.agents, c.ref, c.existing, c.hooks); got != c.want {
			t.Errorf("%s: installNeedsAuth = %v, want %v", c.name, got, c.want)
		}
	}
	// …or the machine already holds the project's key.
	if err := keystore.Set(id, "ter_srv_0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	if installNeedsAuth([]string{"cursor"}, "", bound, true) {
		t.Error("a machine that already holds the project's key must not sign in for one")
	}
}
