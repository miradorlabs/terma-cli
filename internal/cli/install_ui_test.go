package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Default output keeps the project, warnings, and actions; -v includes setup details.
func TestInstallIsConciseUnlessVerbose(t *testing.T) {
	installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--adapters", "claude", "--yes")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{
		"✓ Team", "✓ Hooks", "! Hook events   held until this machine has a key",
		"Almost done", "• Sign in with `terma setup`", "Commit the new files", "git add .terma .claude/settings.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the checklist should say %q:\n%s", want, out)
		}
	}
	for _, detail := range []string{"✓ Repo policy", "create  ", "Wrote Claude Code's repository policy", "Pointed git at"} {
		if strings.Contains(out, detail) {
			t.Errorf("%q is detail, for --verbose:\n%s", detail, out)
		}
	}

	installRepo(t)
	out, err = runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--adapters", "claude", "--yes", "-v")
	if err != nil {
		t.Fatalf("install --verbose: %v\n%s", err, out)
	}
	for _, want := range []string{"✓ Team", "✓ Hooks", "✓ Repo policy", "create  ", "Wrote Claude Code's repository policy", "Pointed git at"} {
		if !strings.Contains(out, want) {
			t.Errorf("--verbose should say %q:\n%s", want, out)
		}
	}
}

func TestInstallUIFinish(t *testing.T) {
	var buf bytes.Buffer
	ui := newInstallUI(&buf, false)
	ui.Summary("Team", "Acme Web")
	ui.OK("Status line", "reads your plan's usage windows")
	ui.OK("Claude Code", "exports to the local relay")
	ui.OK("Hook events", "delivered with this project's key")
	fmt.Fprintln(ui.detail, "only with --verbose")
	ui.Then("Run `source ~/.zshrc`.")
	ui.Then("Commit the new files:\n  git add a")
	ui.Then("Run `source ~/.zshrc`.") // said once
	ui.finish()
	want := "\n  ✓ Team          Acme Web\n\n✓ terma installed\n\nNext steps:\n" +
		"  • Run `source ~/.zshrc`.\n" +
		"  • Commit the new files:\n      git add a\n"
	if got := buf.String(); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	buf.Reset()
	ui = newInstallUI(&buf, true)
	if ui.detail == io.Discard {
		t.Fatal("--verbose discards the detail")
	}
	ui.Warn("Codex", "hooks await trust in Codex")
	ui.finish()
	if !strings.Contains(buf.String(), "! Codex") || !strings.Contains(buf.String(), "! terma installed — the steps marked ! need you") || strings.Contains(buf.String(), "Next steps") {
		t.Fatalf("a warning is the verdict, and no steps means no list:\n%s", buf.String())
	}
}

// A run that stops early still shows what already succeeded, once.
func TestInstallUIPrintsTheChecklistWhenStoppedEarly(t *testing.T) {
	var buf bytes.Buffer
	ui := newInstallUI(&buf, false)
	ui.Summary("Team", "Acme Web")
	ui.printLines()
	ui.printLines()
	if got := buf.String(); got != "\n  ✓ Team          Acme Web\n" {
		t.Fatalf("got %q", got)
	}
}
