package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/keystore"
)

func TestDesktopOnlySelectionDoesNotRequireCodexCLIShim(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if err := routing.SaveRecord(routing.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai", Signals: []string{"logs"},
		Harnesses: []string{"codex"}, Surfaces: []string{codexDesktopAgent}}); err != nil {
		t.Fatal(err)
	}
	selected := selectedForRepo(testProjectID, []string{"codex"})
	if !slices.Equal(selected, []string{codexDesktopAgent}) {
		t.Fatalf("effective choices = %v", selected)
	}
	verdicts := judgeSelectedHarnesses(context.Background(), "https://otel.terma.ai", testProjectID, "", selected)
	var names []string
	for _, verdict := range verdicts {
		names = append(names, verdict.name)
	}
	if slices.Contains(names, "codex") || !slices.Contains(names, codexDesktopAgent) {
		t.Fatalf("desktop-only choice produced agent verdicts %v", names)
	}
}

func TestDesktopChoiceCountsCodexHookTrust(t *testing.T) {
	repo := installRepo(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "codex", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("wire Codex hooks: %v\n%s", err, out)
	}
	check := agentHooksCheck(repo, []string{codexDesktopAgent})
	if check.Of != 1 || check.Status != doctor.Warn {
		t.Fatalf("desktop choice did not check the required Codex hook trust: %+v", check)
	}
}

func TestDesktopVerdictUsesLocalRouteAndKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(home, "terma"))
	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai",
		Signals: []string{"logs"}, Harnesses: []string{"codex"},
		IncludePrompts: true, IncludeToolContent: true, Surfaces: []string{codexDesktopAgent}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := judgeSurface(codexDesktopAgent, "", testProjectID); got.emissionProblem != "this repository has no delivery key" {
		t.Fatalf("missing key verdict = %q", got.emissionProblem)
	}
	if err := keystore.SetFor("codex", testProjectID, "ter_srv_0123456789abcdef01234567", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if verdict, _ := judgeSurface(codexDesktopAgent, "", testProjectID); verdict.emissionProblem != "" || verdict.route != routeHooks {
		t.Fatalf("local desktop verdict = %+v", verdict)
	}
}

// A record an earlier build wrote names no surface: the desktop verdict is still judged,
// and says to install again, rather than disappearing with the choice.
func TestARecordWithoutSurfacesStillJudgesTheDesktop(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if err := routing.SaveRecord(routing.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai", Signals: []string{"logs"},
		Harnesses: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	for _, v := range judgeSelectedHarnesses(context.Background(), "https://otel.terma.ai", testProjectID, "", []string{codexDesktopAgent}) {
		if v.name == codexDesktopAgent {
			if v.emissionFix == "" || !strings.Contains(v.emissionFix, "terma install") {
				t.Fatalf("desktop verdict without a surface route = %+v", v)
			}
			return
		}
	}
	t.Fatal("the desktop verdict disappeared")
}
