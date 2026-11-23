package e2e

import (
	"bytes"
	"cmp"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Grok Build (xAI's grok CLI) exports natively: its OTLP logs, which setup points at the
// relay through [telemetry] in $GROK_HOME/config.toml; terma's hooks file
// ($GROK_HOME/hooks/terma.json) claims each session and spools its prompts, subagents and
// edits. Every run here is in a scratch GROK_HOME inside the sandbox, with a custom model
// whose base_url is a local OpenAI-compatible stub: grok runs with no login and no key, and
// with remote_fetch off its export starts at once. The builds are the installed one, or
// TERMA_E2E_GROK_VERSIONS's, from npm (@xai-official/grok, whose platform binary is an
// optional dependency).

// grokPackage is grok's npm package, which lists its releases.
const grokPackage = "@xai-official/grok"

// grokModel is the custom model the stub serves, by the name grok's config gives it.
const grokModel = "fake-m"

func forEachGrok(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	skipUnlessSelected(t, "grok")
	spec := cmp.Or(os.Getenv("TERMA_E2E_GROK_VERSIONS"), "installed")
	builds := resolve(t, "grok", spec, func() ([]string, error) { return npmReleases(grokPackage) }, ensureGrok)
	if len(builds) == 0 {
		Record(t.Name(), "not run", "grok is not installed")
		t.Skip("grok is not installed")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { scenarioOfBuild(t, b); run(t, b) })
	}
}

// ensureGrok installs a grok build from npm. Its postinstall links the binary into
// $GROK_HOME/bin and records the installer in $GROK_HOME/config.toml, so the install runs
// with a home of its own beside the build, never the developer's ~/.grok.
func ensureGrok(version string) (string, error) {
	if err := CheckPathName("grok version", version); err != nil {
		return "", err
	}
	home, err := filepath.Abs(filepath.Join(versionsDir(), "grok", version, "install-home"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	return ensureNpmBuild("grok", grokPackage, "grok", "HOME="+home, "GROK_HOME="+filepath.Join(home, ".grok"))(version)
}

// grokHome is the sandbox's GROK_HOME.
func (sb *Sandbox) grokHome() string { return filepath.Join(sb.Home, ".grok") }

// UseGrok gives the sandbox's grok a home of its own, never the developer's ~/.grok, whose
// config.toml names the stub at url as its only model, with grok's remote settings fetch off.
// grok's compatibility hooks are off: the sandbox's Claude Code settings hold terma's Claude
// Code hooks, which stand down inside grok and would only add processes. It must run before
// setup, which adds [telemetry] to this file.
func (sb *Sandbox) UseGrok(url string) {
	sb.ExtraEnv = append(sb.ExtraEnv, "GROK_HOME="+sb.grokHome(), "GROK_CLAUDE_HOOKS_ENABLED=0", "GROK_CURSOR_HOOKS_ENABLED=0",
		"GROK_CLAUDE_MCPS_ENABLED=0", "GROK_CURSOR_MCPS_ENABLED=0", "GROK_DISABLE_AUTOUPDATER=1")
	sb.writeAbs(filepath.Join(sb.grokHome(), "config.toml"), `[models]
default = "`+grokModel+`"

[model.`+grokModel+`]
model = "`+grokModel+`"
base_url = "`+url+`/v1"
api_key = "synthetic"
context_window = 128000

[features]
remote_fetch = false
`)
}

// GrokRun runs one `grok -p` in dir, approving every tool.
func (sb *Sandbox) GrokRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "-p", prompt, "-m", grokModel, "--always-approve", "--output-format", "json")
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("grok -p: %v\n%s", err, out.String())
	}
	return out.String()
}
