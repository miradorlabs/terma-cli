package live

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Pi (@earendil-works/pi-coding-agent) has no OpenTelemetry of its own: terma's
// extension (internal/agents/internal/pifamily/terma.ts), written into Pi's agent directory, is its
// exporter and calls `terma hook pi-*`. `terma relay setup --harness pi` writes it
// pointed at the relay; a direct run splices the same template pointed at the receiver,
// so the two differ only in where the extension sends. Only the installed build is
// tested.

func forEachPi(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
	}
	path, err := exec_LookPath("pi")
	if err != nil {
		Record(t.Name(), "not run", "pi is not installed")
		t.Skip("pi is not installed")
	}
	b := Binary{Harness: "pi", Version: numberOf(Version(path)), Path: path, Installed: true}
	t.Run(b.Label(), func(t *testing.T) { run(t, b) })
}

// piAgentDir is the sandbox's Pi configuration directory (PI_CODING_AGENT_DIR's
// default under the sandbox home).
func (sb *Sandbox) piAgentDir() string { return filepath.Join(sb.Home, ".pi", "agent") }

// UsePiProvider points the sandbox's Pi at an OpenAI-compatible endpoint, as its only
// model.
func (sb *Sandbox) UsePiProvider(url string) {
	cfg := map[string]any{"providers": map[string]any{"fake": map[string]any{
		"baseUrl": url + "/v1", "api": "openai-completions", "apiKey": "synthetic",
		"models": []any{map[string]any{"id": "m"}},
	}}}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	sb.writeAbs(filepath.Join(sb.piAgentDir(), "models.json"), string(data)+"\n")
}

// UsePiExtensionDirect writes terma's Pi extension pointed straight at the receiver,
// as the direct half of a comparison.
func (sb *Sandbox) UsePiExtensionDirect() {
	sb.T.Helper()
	sb.writePiFamilyExtension(filepath.Join(sb.piAgentDir(), "extensions", "terma.ts"), "pi", true)
}

// writePiFamilyExtension splices the Pi-family extension template (Pi, omp) pointed at
// the receiver into path.
func (sb *Sandbox) writePiFamilyExtension(path, agent string, lifecycle bool) {
	t := sb.T
	t.Helper()
	tmpl, err := os.ReadFile(filepath.Join("..", "..", "internal", "agents", "internal", "pifamily", "terma.ts"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "const CONFIG: TermaConfig | null = null /* terma:config */"
	cfg, _ := json.Marshal(map[string]any{
		"version": 1, "agent": agent, "lifecycle": lifecycle, "endpoint": sb.Receiver.URL(), "headers": map[string]string{"Authorization": "Bearer " + liveKey},
		"includePrompts": true, "includeToolContent": true, "hookCommand": []string{sb.Terma, "hook"},
	})
	if !bytes.Contains(tmpl, []byte(marker)) {
		t.Fatal("the Pi extension template has no configuration line")
	}
	sb.writeAbs(path, strings.Replace(string(tmpl), marker, "const CONFIG: TermaConfig | null = "+string(cfg)+" /* terma:config */", 1))
}

// PiRun runs one `pi -p` in dir (the live tests run from test/live/, next to the template
// UsePiExtensionDirect reads) and returns its output.
func (sb *Sandbox) PiRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "-p", "--model", "fake/m", "--offline", "--no-context-files", "--no-skills", prompt)
	cmd.Dir = dir
	cmd.Env = append(sb.termaEnv(), "PI_CODING_AGENT_DIR="+sb.piAgentDir())
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pi -p: %v\n%s", err, out.String())
	}
	return out.String()
}
