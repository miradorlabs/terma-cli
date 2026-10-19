package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// GitHub Copilot CLI (@github/copilot) exports traces and metrics from its environment
// alone, which `terma launch copilot` sets as the shell function setup writes does, and
// runs terma's hooks from its user-level hooks directory (internal/agents/copilot).
// Builds come from npm. It has no local provider fixture: these scenarios run against
// GitHub's service with COPILOT_GITHUB_TOKEN, and are not run without it.

// CopilotBinaries resolves the Copilot CLI builds to test (TERMA_E2E_COPILOT_VERSIONS,
// the same spec as the others'). The default leaves out "installed": VS Code puts a
// `copilot` on PATH that only offers to install the CLI.
func CopilotBinaries(t *testing.T) []Binary {
	t.Helper()
	spec := cmp.Or(os.Getenv("TERMA_E2E_COPILOT_VERSIONS"), "last1")
	return resolve(t, "copilot", spec, func() ([]string, error) { return npmReleases("@github/copilot") }, ensureCopilot)
}

func forEachCopilot(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	if os.Getenv("COPILOT_GITHUB_TOKEN") == "" {
		t.Skip("no COPILOT_GITHUB_TOKEN: Copilot CLI runs only against GitHub's service")
	}
	builds := CopilotBinaries(t)
	if len(builds) == 0 {
		t.Skip("no Copilot CLI build to test")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { run(t, b) })
	}
}

// ensureCopilot installs one Copilot CLI release under the versions directory.
func ensureCopilot(version string) (string, error) {
	dir := filepath.Join(versionsDir(), "copilot", version)
	bin := filepath.Join(dir, "node_modules", ".bin", "copilot")
	if _, err := os.Stat(bin); err == nil {
		return abs(bin), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "install", "--silent", "--no-audit", "--no-fund", "--prefix", dir, "@github/copilot@"+version)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm install @github/copilot@%s: %v\n%s", version, err, out)
	}
	return abs(bin), nil
}

// UseCopilot makes b the `copilot` on the sandbox's PATH, signed in by the token, with
// its own home, and sets the machine up again with Copilot chosen: its hooks are the
// machine-wide ones only `terma setup` writes, and its exporter the shell function
// setup defines for the login shell, which is zsh here on every platform.
func (sb *Sandbox) UseCopilot(b Binary) {
	t := sb.T
	t.Helper()
	if err := os.Symlink(b.Path, filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "copilot")); err != nil {
		t.Fatal(err)
	}
	sb.ExtraEnv = append(sb.ExtraEnv, "COPILOT_GITHUB_TOKEN="+os.Getenv("COPILOT_GITHUB_TOKEN"),
		"COPILOT_HOME="+filepath.Join(sb.Home, ".copilot"), "SHELL=/bin/zsh")
	sb.SetupAgents = append(sb.SetupAgents, "copilot")
	sb.RelayAgents = append(sb.RelayAgents, "copilot")
	sb.Setup(nil, "--team", sb.ProjectID, "--harness", sb.setupList())
	sb.Receiver.Reset()
}

var copilotResume = regexp.MustCompile(`--resume=([0-9a-f-]{36})`)

// CopilotTools names the tools session ran, from the runtime's own session log
// (session-state/<id>/events.jsonl), and the model each turn used: what a failure needs
// to say which tool or model terma's hooks did not expect.
func (sb *Sandbox) CopilotTools(session string) string {
	data, err := os.ReadFile(filepath.Join(sb.Home, ".copilot", "session-state", session, "events.jsonl"))
	if err != nil {
		return "no session log: " + err.Error()
	}
	var seen []string
	for line := range strings.SplitSeq(string(data), "\n") {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				ToolName string `json:"toolName"`
				Model    string `json:"model"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if name := cmp.Or(ev.Data.ToolName, ev.Data.Model); name != "" && !slices.Contains(seen, ev.Type+"="+name) {
			seen = append(seen, ev.Type+"="+name)
		}
	}
	return strings.Join(seen, ", ")
}

// CopilotRun runs one headless `copilot -p` in dir, tools approved, the way the shell
// function starts it, and returns the session id it reports and what it printed.
func (sb *Sandbox) CopilotRun(dir, prompt string) (session, output string) {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	terma := filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "terma")
	cmd := exec.CommandContext(ctx, terma, "launch", "copilot", "--", "-p", prompt, "--allow-all-tools", "--no-color")
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("copilot -p: %v\n%s", err, tail(out.String(), 3000))
	}
	if m := copilotResume.FindStringSubmatch(out.String()); m != nil {
		session = m[1]
	}
	return session, out.String()
}
