package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// testApp is the command line every test runs, with this build's agents and no other termas.
var testApp = func() *App {
	app := New(builtin.Agents(), "dev")
	app.binDirs = func() []string { return nil }
	return app
}()

// TestMain gives the package a private HOME so no test can rewrite the developer's agent
// configuration; Go's caches stay put, or every test build would recompile the world.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	if out, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output(); err == nil {
		vals := strings.Split(strings.TrimSpace(string(out)), "\n")
		for i, k := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
			if i < len(vals) && vals[i] != "" {
				_ = os.Setenv(k, vals[i])
			}
		}
	}
	home, err := os.MkdirTemp("", "terma-cmd-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "private home:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(home) }()
	for k, v := range map[string]string{
		"HOME":              home,
		"SHELL":             "/bin/zsh",
		"XDG_CONFIG_HOME":   home + "/.config",
		"CLAUDE_CONFIG_DIR": home + "/.claude",
		"CODEX_HOME":        home + "/.codex",
		"TERMA_CONFIG_DIR":  home + "/.config/terma",
		"GEMINI_CLI_HOME":   home,
		// Offline; policy integration tests clear this and use the real HTTP path.
		"TERMA_POLICY_STUB": `{"mode":"repo","include_prompts":true,"include_tool_content":true}`,
	} {
		_ = os.Setenv(k, v)
	}
	return m.Run()
}

// newTestRelay is relay.New with the registered agents' telemetry shapes, as relay run has.
func newTestRelay(o relay.Options) *relay.Relay {
	o.Correlators, o.Capturers = testApp.agents.With[shape.Correlator](), testApp.agents.With[shape.Capturer]()
	return relay.New(o)
}

func hooksPathOf(name string) string {
	a, _ := testApp.agents.Lookup(name)
	return a.HooksPath()
}

func harnessOf(t *testing.T, name string) harness.Harness {
	t.Helper()
	h, err := testApp.agents.Harness(name)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// claudeHarness is the status-line agent's harness with the capabilities these tests use.
func claudeHarness(t *testing.T) struct {
	harness.Scoped
	harness.Credentialed
	agents.StatusLiner
} {
	t.Helper()
	h := harnessOf(t, "claude")
	scoped, ok := h.(harness.Scoped)
	credentialed, ok2 := h.(harness.Credentialed)
	line, ok3 := testApp.agents.Find[agents.StatusLiner]("claude")
	if !ok || !ok2 || !ok3 {
		t.Fatal("claude lost a capability")
	}
	return struct {
		harness.Scoped
		harness.Credentialed
		agents.StatusLiner
	}{scoped, credentialed, line}
}

const codexDesktopAgent = "codex-desktop"
