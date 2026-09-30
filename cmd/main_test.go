package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// TestMain gives the whole package a private home before any test runs. Commands write
// to the developer's agent configuration — `terma setup` points Codex's and Claude's
// exporters at the relay — and one test that sandboxed only terma's config directory
// rewrote the real ~/.codex/config.toml. A test that wants a particular home still sets
// its own (sandboxMachine); none can reach the real one by forgetting to.
//
// Go's caches stay where they are: under a fresh HOME, every `go build` a test runs
// (termaBinary) would download and compile the world again.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	registered = builtin.Agents()
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
		// Command unit tests are offline; policy integration tests explicitly clear
		// this override and exercise the real authenticated HTTP path.
		"TERMA_POLICY_STUB": `{"mode":"repo","include_prompts":true,"include_tool_content":true}`,
	} {
		_ = os.Setenv(k, v)
	}
	return m.Run()
}

// newTestRelay is relay.New with the registered agents' telemetry shapes, as relay run has.
func newTestRelay(o relay.Options) *relay.Relay {
	o.Correlators, o.Capturers = registered.With[shape.Correlator](), registered.With[shape.Capturer]()
	return relay.New(o)
}
