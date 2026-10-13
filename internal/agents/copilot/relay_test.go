package copilot

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// With the CLI installed, setup defines the function that starts it through terma, in the
// shell's startup file and holding nothing of the relay's; the kill switch and the undo
// each take it out again.
func TestSetupStartsTheCLIThroughTerma(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell startup files on Windows")
	}
	a, _ := sandbox(t)
	home, _ := os.UserHomeDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, command), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(home, ".zshrc")
	holds := func() bool {
		data, _ := os.ReadFile(rc)
		if strings.Contains(string(data), relay.Token) || strings.Contains(string(data), "43180") {
			t.Fatalf("the startup file holds the relay's address or token:\n%s", data)
		}
		return strings.Contains(string(data), "terma launch copilot --")
	}
	ctx := context.Background()

	// Not installed: nothing to start, so nothing is written, and nothing is wrong.
	t.Setenv("SHELL", "/bin/zsh")
	if _, err := a.ConfigureRelay(ctx, relay); err != nil || holds() {
		t.Fatalf("configure without the CLI: %v", err)
	}
	if _, _, problem := a.RelayProblem(); problem {
		t.Fatal("a problem with no CLI to start")
	}

	t.Setenv("PATH", bin)
	if detail, fix, problem := a.RelayProblem(); !problem || !strings.Contains(detail, "through terma") || fix != "terma setup" {
		t.Fatalf("RelayProblem before setup = %q, %q, %v", detail, fix, problem)
	}
	res, err := a.ConfigureRelay(ctx, relay)
	if err != nil || !holds() || !slices.Contains(res.Paths, rc) {
		t.Fatalf("configure: %+v, %v", res, err)
	}
	if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "new terminal") }) {
		t.Errorf("notes do not say the function needs a new shell: %q", res.Notes)
	}
	if _, _, problem := a.RelayProblem(); problem {
		t.Fatal("a problem after setup")
	}
	set, unset := a.LaunchEnv(relay)
	if set[harness.EnvOTLPEndpoint] != "http://127.0.0.1:43180/"+relay.Token || set["COPILOT_OTEL_ENABLED"] != "true" ||
		set["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] != "true" || !slices.Contains(unset, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") {
		t.Fatalf("LaunchEnv = %v, %v", set, unset)
	}
	if _, has := set[harness.EnvOTLPHeaders]; has {
		t.Error("the token is in a header a per-signal endpoint would also receive")
	}
	if again, err := a.ConfigureRelay(ctx, relay); err != nil || len(again.Notes) > 0 {
		t.Fatalf("a second configure: %+v, %v", again, err)
	}

	t.Setenv("TERMA_SHELL_LAUNCHERS", "0")
	if _, err := a.ConfigureRelay(ctx, relay); err != nil || holds() {
		t.Fatalf("configure under the kill switch: %v", err)
	}
	if detail, _, problem := a.RelayProblem(); !problem || !strings.Contains(detail, "TERMA_SHELL_LAUNCHERS") {
		t.Fatalf("RelayProblem under the kill switch = %q, %v", detail, problem)
	}

	t.Setenv("TERMA_SHELL_LAUNCHERS", "")
	if _, err := a.ConfigureRelay(ctx, relay); err != nil || !holds() {
		t.Fatalf("configure again: %v", err)
	}
	undone, err := a.UnconfigureRelay(ctx, relay.Token)
	if err != nil || holds() || !slices.Contains(undone.Paths, rc) {
		t.Fatalf("unconfigure: %+v, %v", undone, err)
	}
}

// A shell terma writes no function for is said, since the CLI then exports nothing.
func TestSetupSaysWhenItCannotStartTheCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell startup files on Windows")
	}
	a, _ := sandbox(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, command), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	res, err := a.ConfigureRelay(context.Background(), relay)
	if err != nil || !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "zsh, bash and fish") }) {
		t.Fatalf("configure under an unknown shell: %+v, %v", res, err)
	}
	if _, fix, problem := a.RelayProblem(); !problem || !strings.Contains(fix, "zsh, bash or fish") {
		t.Fatalf("doctor's fix under an unknown shell = %q, %v", fix, problem)
	}
}
