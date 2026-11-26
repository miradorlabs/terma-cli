package doctor

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func TestReadinessSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		checks []Check
		want   string
		failed bool
	}{
		{"ready", []Check{{Status: Pass}}, "Setup ready", false},
		{"partial export", []Check{{Status: Warn, Ready: 1, Of: 2, Fix: "open a new terminal"}}, "open a new terminal", false},
		{"unverified backend", []Check{{Key: KeyBackend, Status: Warn, Inconclusive: true, Fix: "terma doctor"}}, "terma doctor", false},
		{"broken backend", []Check{{Key: KeyBackend, Status: Fail, Fix: "check network"}}, "check network", true},
		{"skipped probe", []Check{{Key: KeyBackend, Status: Skip}}, "Verification incomplete", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Build(tc.checks)
			var out bytes.Buffer
			RenderSummary(&out, r)
			if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "%") || r.Failed() != tc.failed {
				t.Fatalf("summary=%q failed=%v", out.String(), r.Failed())
			}
		})
	}
}

func TestReadinessDeduplicatesActions(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	RenderSummary(&out, Build([]Check{
		{Status: Warn, Fix: "open a new terminal"},
		{Status: Warn, Fix: "open a new terminal"},
		{Status: Fail, Fix: "terma setup"},
	}))
	if strings.Count(out.String(), "open a new terminal") != 1 || !strings.Contains(out.String(), "terma setup") {
		t.Fatal(out.String())
	}
}

// A check's line holds its name and detail; a failure's fix waits for the summary, which
// gives a command a line of its own.
func TestRenderCheckAndSummary(t *testing.T) {
	t.Parallel()
	c := Check{Status: Fail, Name: "commit hooks in effect", Detail: "hooks missing", Fix: "terma setup"}
	var out bytes.Buffer
	RenderCheck(&out, c)
	if got := out.String(); got != "  ✗ commit hooks in effect    hooks missing\n" {
		t.Fatalf("check line = %q", got)
	}
	out.Reset()
	RenderSummary(&out, Build([]Check{c}))
	if got, want := out.String(), "\nSetup needs attention\n  ✗ commit hooks in effect\n    terma setup\n"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

// A fix's command is drawn only on a terminal; plain output is the fix as written.
func TestFixTextIsPlainOffATerminal(t *testing.T) {
	t.Parallel()
	for _, fix := range []string{"terma setup", "terma setup (a later line puts the real binaries back in front)", "run `source ~/.zshrc` or open a new terminal"} {
		if got := fixText(style.Plain(), fix); got != fix {
			t.Errorf("fixText(%q) = %q", fix, got)
		}
	}
}

// Context is doctor's header: it names a non-production backend and the directories, and
// outside a repository says only that.
func TestContextNamesTheDirectoriesAndTheBackendOutsideARepository(t *testing.T) {
	t.Parallel()
	if s := Context(Env{}); s != nil {
		t.Fatalf("no configuration: %v", s)
	}
	configDir, stateDir := t.TempDir(), t.TempDir()
	s := Context(Env{ConfigDir: configDir, StateDir: stateDir, Config: &config.Config{Environment: config.EnvDev, AuthURL: "https://auth.example"}, RepoErr: errors.New("not a repository")})
	if len(s) != 2 || s[0].Title != "This machine" || len(s[0].Lines) != 3 ||
		s[0].Lines[0].Key != "environment" || !strings.Contains(s[0].Lines[0].Text, "https://auth.example") ||
		s[0].Lines[1] != (Line{Key: "config", Text: output.TildePath(configDir)}) || s[0].Lines[2] != (Line{Key: "state", Text: output.TildePath(stateDir)}) {
		t.Fatalf("machine = %+v", s)
	}
	if repo := s[1]; repo.Title != "This repository" || len(repo.Lines) != 1 || repo.Lines[0] != (Line{Mark: MarkFail, Text: "not a repository"}) {
		t.Fatalf("repository = %+v", repo)
	}
}

// At a terminal's width every line wraps under its own column, except a command, which
// keeps a line of its own, whole, so it copies as it is.
func TestPrinterWrapsToTheTerminalButNeverACommand(t *testing.T) {
	t.Parallel()
	cmd := `echo 'export PATH="$HOME/VSCodeProject/terma-cli-setup-flow/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc`
	checks := []Check{
		{Status: Warn, Name: "terma on PATH", Detail: "PATH finds another build at ~/.local/bin/terma (build bea38e0), which is not the one the hooks run on this machine.",
			Fix: "Put this build first on PATH.\n`" + cmd + "`"},
		{Status: Skip, Name: "backend receives events", Detail: "nothing was waiting to send, so delivery was not tested"},
	}
	var b strings.Builder
	pr := &Printer{W: &b, P: style.Plain(), Cols: 80}
	pr.Sections([]Section{{Title: "Teams", Lines: []Line{
		{Mark: MarkOK, Text: "1. Mirador Dev › Platform", Note: "updated 26 Nov 09:07"},
		{Depth: 1, Text: "github.com/miradorlabs/mirador-platform"},
		{Depth: 2, Quiet: true, Text: "Also listed by Engineering. This policy is newer."},
	}, Footnote: orderNote}})
	for _, c := range checks {
		pr.Check(c)
	}
	pr.Summary(Build(checks))
	out := b.String()
	for l := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if runes(l) > 80 && strings.TrimSpace(l) != cmd {
			t.Errorf("line over 80 columns: %q", l)
		}
	}
	for _, want := range []string{
		"  ! terma on PATH             PATH finds another build at ~/.local/bin/terma\n                              (build bea38e0), which is not the one the hooks\n",
		"\nSetup needs attention\n  ! terma on PATH\n    Put this build first on PATH.\n    " + cmd + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "PATH finds another build") != 1 {
		t.Errorf("the summary repeats the check line:\n%s", out)
	}
}
