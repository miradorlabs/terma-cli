package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// DeepSeek Harness (dsh, @deepseek-ai/dsh) sends its own OTLP to DeepSeek; terma's Cordis
// plugin (internal/agents/dsh/plugin/terma.mjs) is its exporter and calls `terma hook dsh-*`.
// Builds come from npm (every release so far is a prerelease, so the dist-tag `latest`
// is what is tested); its native adapter speaks the Anthropic Messages API at
// DEEPSEEK_BASE_URL, so the Claude fakes stand in for DeepSeek.

// DshBinaries resolves the dsh builds to test: TERMA_LIVE_DSH_VERSIONS, else the
// installed one and npm's latest.
func DshBinaries(t *testing.T) []Binary {
	t.Helper()
	spec := os.Getenv("TERMA_LIVE_DSH_VERSIONS")
	if spec == "" {
		spec = "installed,last1"
	}
	return resolve(t, "dsh", spec, dshLatest, ensureDsh)
}

// dshLatest is npm's latest dsh: its releases so far are all prereleases, which the
// release list the others use leaves out.
func dshLatest() ([]string, error) {
	data, err := fetch("https://registry.npmjs.org/@deepseek-ai/dsh")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Tags["latest"] == "" {
		return nil, fmt.Errorf("no latest dsh on npm")
	}
	return []string{doc.Tags["latest"]}, nil
}

func ensureDsh(version string) (string, error) {
	dir := filepath.Join(versionsDir(), "dsh", version)
	bin := filepath.Join(dir, "node_modules", ".bin", "dsh")
	if _, err := os.Stat(bin); err == nil {
		return abs(bin), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "install", "--silent", "--no-audit", "--no-fund", "--prefix", dir, "@deepseek-ai/dsh@"+version)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm install @deepseek-ai/dsh@%s: %v\n%s", version, err, out)
	}
	return abs(bin), nil
}

func forEachDsh(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
	}
	builds := DshBinaries(t)
	if len(builds) == 0 {
		t.Skip("no dsh build to test")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { run(t, b) })
	}
}

// dshHome is the sandbox's DSH_HOME.
func (sb *Sandbox) dshHome() string { return filepath.Join(sb.Home, ".dsh") }

// UseDsh points the sandbox's dsh at an Anthropic-compatible endpoint, with DeepSeek's
// own session telemetry off, in every environment the sandbox builds.
func (sb *Sandbox) UseDsh(url string) {
	sb.ExtraEnv = append(sb.ExtraEnv, "DSH_HOME="+sb.dshHome(), "DEEPSEEK_BASE_URL="+url, "DEEPSEEK_API_KEY=synthetic", "DSH_TELEMETRY_MODE=DISABLED")
}

// UseDshDirect writes terma's plugin pointed straight at the receiver, and inserts it,
// as the direct half of a comparison.
func (sb *Sandbox) UseDshDirect() {
	t := sb.T
	t.Helper()
	tmpl, err := os.ReadFile(filepath.Join("..", "..", "internal", "agents", "dsh", "plugin", "terma.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "const CONFIG = null /* terma:config */"
	cfg, _ := json.Marshal(map[string]any{
		"version": 1, "endpoint": sb.Receiver.URL(), "headers": map[string]string{"Authorization": "Bearer " + liveKey},
		"includePrompts": true, "includeToolContent": true, "hookCommand": []string{sb.Terma, "hook"},
	})
	if !bytes.Contains(tmpl, []byte(marker)) {
		t.Fatal("the dsh plugin template has no configuration line")
	}
	path := filepath.Join(sb.dshHome(), "plugins", "terma-relay.mjs")
	sb.writeAbs(path, strings.Replace(string(tmpl), marker, "const CONFIG = "+string(cfg)+" /* terma:config */", 1))
	sb.writeAbs(filepath.Join(sb.dshHome(), "cordis.patch.yml"), "- insert:\n    - id: terma\n      name: '"+path+"'\n")
}

// DshRun runs one `dsh headless` in dir and returns its output.
func (sb *Sandbox) DshRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "headless", prompt)
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("dsh headless: %v\n%s", err, tail(out.String(), 3000))
	}
	return out.String()
}
