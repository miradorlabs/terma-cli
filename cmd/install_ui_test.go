package cmd

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
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

// install's Verified step: each problem doctor found is a next step — its fix, or its
// name and detail when it names none — and the full report is one command away. The
// routing warning is left out only when a next step already says to reload the shell.
func TestInstallUIVerdict(t *testing.T) {
	report := doctor.Report{Checks: []doctor.Check{
		{Key: doctor.KeyAuth, Name: "signed in", Status: doctor.Pass},
		{Key: doctor.KeyRouting, Name: "shell routing active", Status: doctor.Warn, Detail: "this shell has not activated it", Fix: "run `source ~/.zshrc` or open a new terminal"},
		{Key: doctor.KeyAgentHooks, Name: "agent hooks run", Status: doctor.Warn, Detail: "Codex hooks untrusted", Fix: "open Codex and trust this project's hooks"},
		{Key: doctor.KeyHooks, Name: "commit hooks installed", Status: doctor.Fail, Detail: "git wiring was written by an earlier terma", Fix: "terma update --refresh"},
		{Key: doctor.KeyBackend, Name: "backend receives events", Status: doctor.Fail, Detail: "no event after 30s"},
		{Key: doctor.KeyScratch, Name: "scratch commit stamped", Status: doctor.Skip},
	}}
	var buf bytes.Buffer
	ui := newInstallUI(&buf, false)
	ui.reloading = true
	ui.verdict(report)
	ui.finish()
	out := buf.String()
	for _, want := range []string{
		"! Verified      terma doctor found 3 thing(s) to fix",
		"1. Open Codex and trust this project's hooks",
		"2. Run `terma update --refresh`.",
		"3. backend receives events: no event after 30s",
		"4. Run `terma doctor` for the full report.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the verdict should say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "source ~/.zshrc") || strings.Contains(out, "Codex hooks untrusted") {
		t.Errorf("a reload already asked for, or a detail with a fix, is not repeated:\n%s", out)
	}

	buf.Reset()
	ui = newInstallUI(&buf, false)
	ui.verdict(report)
	if ui.finish(); !strings.Contains(buf.String(), "Run `source ~/.zshrc` or open a new terminal") {
		t.Errorf("with no reload step queued, the routing fix is the developer's:\n%s", buf.String())
	}

	for want, checks := range map[string][]doctor.Check{
		"✓ Verified      terma doctor: all checks passed":                             {{Status: doctor.Pass}},
		"✓ Verified      terma doctor: the checks that ran passed; some were skipped": {{Status: doctor.Pass}, {Key: doctor.KeyBackend, Status: doctor.Skip}},
	} {
		buf.Reset()
		newInstallUI(&buf, false).verdict(doctor.Report{Checks: checks})
		if !strings.Contains(buf.String(), want) {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	}
}
