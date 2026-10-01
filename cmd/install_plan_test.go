package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/install"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/style"
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

// The committed binding says nothing about which agents are wired — that is the hooks
// files' to say, and a list there churned with each colleague's own agents. A binding
// from an install that still wrote one loses it on the next install, and --no-hooks
// writes no agent's hooks file whatever --adapters asks for.
func TestInstallRecordsNoAdapters(t *testing.T) {
	repo := installRepo(t)
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	path := termaproject.Path(repo)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "adapters") {
		t.Fatalf("install recorded adapters in the committed binding:\n%s", data)
	}
	// A binding an older terma wrote, with the list.
	legacy := strings.Replace(string(data), `"install": {`, `"install": {"adapters": ["claude"],`, 1)
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "install", "--harness", "none", "--adapters", "cursor", "--no-hooks", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("re-install --no-hooks: %v\n%s", err, out)
	}
	if data, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "adapters") {
		t.Fatalf("re-install kept the retired adapters field:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cursor", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("--no-hooks wrote cursor hooks (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); err != nil {
		t.Fatalf("--no-hooks re-install removed the committed Claude hooks: %v", err)
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

// The question that asks to write the hooks names the files; the lines under it say what
// each one does and what committing them means, so a developer knows what a yes does.
// Every file the question names gets its own line.
func TestInstallHookQuestionExplainsEachFile(t *testing.T) {
	repo := installRepo(t)
	plan, err := install.PlanHooks(registered, repo, hookmgr.Detect(repo), []string{"claude", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	lines := plan.Explain()
	if len(lines) != len(plan.Files())+2 {
		t.Fatalf("want a line per file %v and the closing sentence, got:\n%s", plan.Files(), strings.Join(lines, "\n"))
	}
	for i, want := range [][2]string{
		{".terma/hooks/", "stamps each commit with the agent session that wrote it"},
		{".claude/settings.json", "reports each Claude Code session and the files it edits"},
		{".codex/hooks.json", "reports each Codex session and the files it edits"},
	} {
		if !strings.HasPrefix(lines[i], want[0]) || !strings.Contains(lines[i], want[1]) {
			t.Errorf("line %d = %q, want %s explained as %q", i, lines[i], want[0], want[1])
		}
	}
	if text := strings.Join(lines, " "); !strings.Contains(text, "merging them sets up everyone who clones") || !strings.Contains(text, "without terma they do nothing") {
		t.Errorf("the explanation should say what committing the files means:\n%s", strings.Join(lines, "\n"))
	}
}

// With detail, the question stands alone, the detail sits indented under it, and the
// answer is typed on a line of its own; without, the answer follows the question.
func TestConfirmPromptPutsTheDetailBeforeTheAnswer(t *testing.T) {
	p := style.Plain()
	if got, want := confirmPrompt(p, "Write them?", nil, true), "? Write them? [Y/n] "; got != want {
		t.Errorf("without detail: %q, want %q", got, want)
	}
	got := confirmPrompt(p, "Write them?", []string{"a.json  does a", "b.json  does b"}, false)
	if want := "? Write them?\n  a.json  does a\n  b.json  does b\n  [y/N] "; got != want {
		t.Errorf("with detail: %q, want %q", got, want)
	}
}
