package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// A hook manager that needs something run in each clone says so in the plan. Without
// these lines a lefthook or pre-commit repository commits hooks that never run.
func TestInstallPrintsWhatTheHookManagerNeedsFromEachClone(t *testing.T) {
	repo := installRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "lefthook.yml"), []byte("pre-commit:\n  commands: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "After merging:") || !strings.Contains(out, "lefthook install") {
		t.Fatalf("the plan does not say what lefthook needs from each clone:\n%s", out)
	}
}

// --dry-run shows the files an install would write, and writes none of them.
func TestInstallDryRunListsTheFilesAndWritesNothing(t *testing.T) {
	repo := installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--dry-run")
	if err != nil {
		t.Fatalf("install --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{".terma/hooks/post-commit", ".claude/settings.json", "Dry run: nothing written."} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry run does not mention %q:\n%s", want, out)
		}
	}
	for _, p := range []string{".terma", ".claude"} {
		if _, err := os.Stat(filepath.Join(repo, p)); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote %s (stat err = %v)", p, err)
		}
	}
}

// A --dry-run must never sign in — sign-in verifies and rewrites the stored credential,
// which "nothing written" forbids. Even with a telemetry harness (which a real install
// would sign in for) and no credential present, the dry run plans and exits cleanly
// rather than trying to log in.
func TestInstallDryRunDoesNotSignIn(t *testing.T) {
	installRepo(t)
	out, err := runTerma(t, "install", "--harness", "claude", "--project", testProjectID,
		"--adapters", "claude", "--dry-run", "--no-browser", "--no-statusline")
	if err != nil {
		t.Fatalf("dry run with a telemetry harness should not require sign-in: %v\n%s", err, out)
	}
	if !strings.Contains(out, "would sign in first") || !strings.Contains(out, "Dry run: nothing written.") {
		t.Fatalf("dry run did not note the skipped sign-in:\n%s", out)
	}
	// No credential store was written by the dry run.
	if _, err := os.Stat(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote credentials (stat err = %v)", err)
	}
}

// A dry run with no stored credential and no --project must still print the plan.
// Skipping sign-in (A4) left the project picker unable to reach the API, which used to
// fail the command before anything was printed — arguably the most common way to try
// `terma install --dry-run`. It plans against an unresolved project instead.
func TestInstallDryRunUnauthenticatedNoProject(t *testing.T) {
	installRepo(t)
	out, err := runTerma(t, "install", "--harness", "claude",
		"--adapters", "claude", "--dry-run", "--no-browser", "--no-statusline")
	if err != nil {
		t.Fatalf("dry run without a credential or --project should still plan: %v\n%s", err, out)
	}
	for _, want := range []string{"unresolved", "would sign in first", "Dry run: nothing written."} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry run output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote credentials (stat err = %v)", err)
	}
}

// The committed binding names only adapters whose hooks file was written: a re-install
// that writes no hooks keeps what was committed instead of adding this developer's agents.
func TestInstallWithoutHooksRecordsOnlyWiredAdapters(t *testing.T) {
	repo := installRepo(t)
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	// Requesting Cursor hooks with --no-hooks writes no .cursor/hooks.json.
	if out, err := runTerma(t, "install", "--harness", "none", "--adapters", "cursor", "--no-hooks", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("re-install --no-hooks: %v\n%s", err, out)
	}
	bound, err := termaproject.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(bound.Install.Adapters, ","); got != "claude" {
		t.Fatalf("adapters = %q, want only the wired one (claude)", got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cursor", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("--no-hooks wrote cursor hooks (stat err = %v)", err)
	}
}

// An install that writes hooks names every file it wrote, so the developer commits them
// all; the hooks do nothing for a colleague until they are merged. A re-run that writes
// nothing asks for no commit.
func TestInstallListsTheFilesToCommit(t *testing.T) {
	installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,cursor", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	_, list, ok := strings.Cut(out, "Commit these files")
	if !ok {
		t.Fatalf("install did not ask for a commit:\n%s", out)
	}
	for _, want := range []string{".terma/hooks/post-commit", ".claude/settings.json", ".cursor/hooks.json", termaproject.FileName, "git add "} {
		if !strings.Contains(list, want) {
			t.Errorf("commit list is missing %s:\n%s", want, list)
		}
	}
	out, err = runTerma(t, "install", "--harness", "none", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("re-install: %v\n%s", err, out)
	}
	if strings.Contains(out, "Commit these files") {
		t.Fatalf("a re-install that wrote nothing asked for a commit:\n%s", out)
	}
}
