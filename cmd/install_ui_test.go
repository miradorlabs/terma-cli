package cmd

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

// install says whether each step worked, one line each, and saves how for --verbose: the
// plan's file list and the policy it wrote are detail, and the files to commit are a next
// step either way.
func TestInstallIsAChecklistUnlessVerbose(t *testing.T) {
	installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{
		"✓ Project", "✓ Hooks         commit stamping via git; session hooks for Claude Code",
		"! Hook events   held until this machine has a key", "✓ Repo policy",
		"terma installed", "Next steps:\n  1. Sign in with `terma setup`", "Commit these files", "git add ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the checklist should say %q:\n%s", want, out)
		}
	}
	for _, detail := range []string{"create  ", "Wrote Claude Code's repository policy", "Pointed git at"} {
		if strings.Contains(out, detail) {
			t.Errorf("%q is detail, for --verbose:\n%s", detail, out)
		}
	}

	installRepo(t)
	out, err = runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor", "--verbose")
	if err != nil {
		t.Fatalf("install --verbose: %v\n%s", err, out)
	}
	for _, want := range []string{"✓ Project", "create  ", "Wrote Claude Code's repository policy", "Pointed git at"} {
		if !strings.Contains(out, want) {
			t.Errorf("--verbose should say %q:\n%s", want, out)
		}
	}
}

func TestInstallUIFinish(t *testing.T) {
	var buf bytes.Buffer
	ui := newInstallUI(&buf, false)
	ui.ok("Project", "Acme Web")
	fmt.Fprintln(ui.detail, "only with --verbose")
	ui.then("Run `source ~/.zshrc`.")
	ui.then("Commit these files:\n  a\n\n  git add a")
	ui.then("Run `source ~/.zshrc`.") // said once
	ui.finish()
	want := "  ✓ Project       Acme Web\n\n✓ terma installed\n\nNext steps:\n" +
		"  1. Run `source ~/.zshrc`.\n" +
		"  2. Commit these files:\n       a\n\n       git add a\n"
	if got := buf.String(); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	buf.Reset()
	ui = newInstallUI(&buf, true)
	if ui.detail == io.Discard {
		t.Fatal("--verbose discards the detail")
	}
	ui.warn("PATH", "the shims are not on PATH yet")
	ui.finish()
	if !strings.Contains(buf.String(), "! PATH") || !strings.Contains(buf.String(), "! terma installed — the steps marked ! need you") || strings.Contains(buf.String(), "Next steps") {
		t.Fatalf("a warning is the verdict, and no steps means no list:\n%s", buf.String())
	}
}

func TestDoctorFixStep(t *testing.T) {
	for fix, want := range map[string]string{
		"terma update --refresh": "Run `terma update --refresh`.",
		"terma install (a later line in ~/.zshrc puts the real binaries back in front)": "Run `terma install` (a later line in ~/.zshrc puts the real binaries back in front).",
		"open Codex and trust this project's hooks":                                     "Open Codex and trust this project's hooks",
	} {
		if got := doctorFixStep(fix); got != want {
			t.Errorf("doctorFixStep(%q) = %q, want %q", fix, got, want)
		}
	}
}
