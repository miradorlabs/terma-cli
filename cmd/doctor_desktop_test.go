package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

func TestDesktopOnlySelectionDoesNotRequireCodexCLIShim(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	cli, desktop := false, true
	if err := shim.SaveRecord(shim.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai", Signals: []string{"logs"},
		Harnesses: []string{shim.AgentCodex}, CLI: cli, Desktop: desktop}); err != nil {
		t.Fatal(err)
	}
	selected := selectedForRepo(testProjectID, []string{shim.AgentCodex})
	if !slices.Equal(selected, []string{codexDesktopAgent}) {
		t.Fatalf("effective choices = %v", selected)
	}
	verdicts := judgeSelectedHarnesses(context.Background(), "https://otel.terma.ai", testProjectID, "", selected)
	var names []string
	for _, verdict := range verdicts {
		names = append(names, verdict.name)
	}
	if slices.Contains(names, shim.AgentCodex) || !slices.Contains(names, codexDesktopAgent) {
		t.Fatalf("desktop-only choice produced agent verdicts %v", names)
	}
	if check := shellRoutingCheck(verdicts, true, selected); check.Status != doctor.Skip {
		t.Fatalf("desktop-only choice demanded a shell shim: %+v", check)
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
	desktop := true
	if err := shim.SaveRecord(shim.Record{ProjectID: testProjectID, Endpoint: "https://otel.terma.ai",
		Signals: []string{"logs"}, Harnesses: []string{shim.AgentCodex},
		IncludePrompts: true, IncludeToolContent: true, Desktop: desktop}); err != nil {
		t.Fatal(err)
	}
	if got := judgeDesktop(testProjectID).emissionProblem; got != "this repository has no delivery key" {
		t.Fatalf("missing key verdict = %q", got)
	}
	if err := keystore.SetFor(shim.AgentCodex, testProjectID, "ter_srv_0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	if verdict := judgeDesktop(testProjectID); verdict.emissionProblem != "" || !verdict.routed {
		t.Fatalf("local desktop verdict = %+v", verdict)
	}
}
