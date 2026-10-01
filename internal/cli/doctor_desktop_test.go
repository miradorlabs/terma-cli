package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/doctor"
)

func TestDesktopOnlySelectionJudgesOnlyCodexDesktop(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if err := routing.SaveRecord(routing.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai", Signals: []string{"logs"},
		Harnesses: []string{"codex"}, Surfaces: []string{codexDesktopAgent}}); err != nil {
		t.Fatal(err)
	}
	selected := doctor.SelectedForRepo(testApp.agents, testProjectID, []string{"codex"})
	if !slices.Equal(selected, []string{codexDesktopAgent}) {
		t.Fatalf("effective choices = %v", selected)
	}
	verdicts := doctor.JudgeSelectedHarnesses(context.Background(), testApp.agents, storedKeys, "https://otel.terma.ai", testProjectID, "", selected)
	var names []string
	for _, verdict := range verdicts {
		names = append(names, verdict.Name)
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
	check := doctor.AgentHooksCheck(testApp.agents, repo, []string{codexDesktopAgent})
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
	if got, _ := doctor.JudgeSurface(testApp.agents, storedKeys, codexDesktopAgent, "", testProjectID); got.EmissionProblem != "this repository has no delivery key" {
		t.Fatalf("missing key verdict = %q", got.EmissionProblem)
	}
	if err := keystore.SetFor("codex", testProjectID, "ter_srv_0123456789abcdef01234567", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if verdict, _ := doctor.JudgeSurface(testApp.agents, storedKeys, codexDesktopAgent, "", testProjectID); verdict.EmissionProblem != "" || verdict.Route != doctor.RouteHooks {
		t.Fatalf("local desktop verdict = %+v", verdict)
	}
}

// A record that names no surface still gets a desktop verdict that says to install again.
func TestARecordWithoutSurfacesStillJudgesTheDesktop(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if err := routing.SaveRecord(routing.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai", Signals: []string{"logs"},
		Harnesses: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	for _, v := range doctor.JudgeSelectedHarnesses(context.Background(), testApp.agents, storedKeys, "https://otel.terma.ai", testProjectID, "", []string{codexDesktopAgent}) {
		if v.Name == codexDesktopAgent {
			if v.EmissionFix == "" || !strings.Contains(v.EmissionFix, "terma install") {
				t.Fatalf("desktop verdict without a surface route = %+v", v)
			}
			return
		}
	}
	t.Fatal("the desktop verdict disappeared")
}
