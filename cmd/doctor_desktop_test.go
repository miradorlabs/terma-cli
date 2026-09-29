package cmd

import (
	"context"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Codex Desktop reads Codex's own global configuration. A developer who chose only the
// desktop app is judged by it, under the app's name — though no Codex CLI is on PATH for
// the CLI's verdict to be found by.
func TestDesktopOnlySelectionIsJudgedByCodexsGlobalConfig(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	r := fakeRelay(t)
	r.installed = "terma"
	e := harness.Exporter{Endpoint: r.config.Endpoint(), APIKey: r.config.Token, Signals: harness.AllSignals, JSON: true}
	if err := (harness.Codex{}).Connect(e, false); err != nil {
		t.Fatal(err)
	}
	verdicts := judgeSelectedHarnesses(context.Background(), "https://otel.terma.ai", testProjectID, "", []string{codexDesktopAgent})
	if len(verdicts) != 1 || verdicts[0].name != codexDesktopAgent || verdicts[0].displayName != "Codex Desktop" {
		t.Fatalf("desktop-only choice produced verdicts %+v", verdicts)
	}
	if verdicts[0].route != routeRelay {
		t.Fatalf("desktop exporting through the relay judged %v", verdicts[0].route)
	}
	if check := relayCheck(context.Background(), verdicts); check.Status != doctor.Pass {
		t.Fatalf("relay check = %+v", check)
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
