package doctor

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func TestReadinessSummary(t *testing.T) {
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

func TestRenderCheck(t *testing.T) {
	var out bytes.Buffer
	RenderCheck(&out, Check{Status: Fail, Name: "commit hooks in effect", Detail: "hooks missing", Fix: "terma setup"}, NameWidth)
	if !strings.Contains(out.String(), "FAIL  commit hooks in effect") || !strings.Contains(out.String(), "→ terma setup") {
		t.Fatal(out.String())
	}
}

// A fix's command is drawn only on a terminal; plain output is the fix as written.
func TestFixTextIsPlainOffATerminal(t *testing.T) {
	for _, fix := range []string{"terma setup", "terma setup (a later line puts the real binaries back in front)", "run `source ~/.zshrc` or open a new terminal"} {
		if got := fixText(style.Plain(), fix); got != fix {
			t.Errorf("fixText(%q) = %q", fix, got)
		}
	}
}

// Context is doctor's header: it names a non-production backend, and outside a repository
// it has no work to report and needs no agents.
func TestContextNamesTheBackendOutsideARepository(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if rows := Context(Env{}); rows != nil {
		t.Fatalf("no configuration: %v", rows)
	}
	rows := Context(Env{Config: &config.Config{Environment: config.EnvDev, AuthURL: "https://auth.example"}, RepoErr: errors.New("not a repository")})
	if len(rows) != 1 || rows[0].Label != "Environment" || !strings.Contains(rows[0].Value, "https://auth.example") {
		t.Fatalf("rows = %v", rows)
	}
}
