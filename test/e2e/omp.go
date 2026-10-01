package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// omp (oh-my-pi) has a native exporter, but it reads its OTEL_* variables once at
// startup, before any extension loads, so only the process that launches omp could set them. Through
// the relay, omp runs terma's Pi-family extension instead (internal/agents/internal/pifamily/terma.ts,
// agent "omp", in ~/.omp/agent/extensions), which exports from omp's own events; its
// sessions are claimed by the committed hook file `terma install --adapters omp` writes
// and the extension's claim-only omp-prompt. The direct half of a comparison runs the
// same extension pointed at the receiver. Only the installed build is tested: the npm
// package is over a gigabyte.

func forEachOmp(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	path, err := exec_LookPath("omp")
	if err != nil {
		Record(t.Name(), "not run", "omp is not installed")
		t.Skip("omp is not installed")
	}
	b := Binary{Harness: "omp", Version: numberOf(Version(path)), Path: path, Installed: true}
	t.Run(b.Label(), func(t *testing.T) { run(t, b) })
}

// UseOmpProvider points the sandbox's omp at an OpenAI-compatible endpoint.
func (sb *Sandbox) UseOmpProvider(url string) {
	sb.writeAbs(filepath.Join(sb.Home, ".omp", "agent", "models.yml"), `providers:
  fake:
    baseUrl: `+url+`/v1
    api: openai-completions
    apiKey: synthetic
    models:
      - id: m
`)
}

// UseOmpExtensionDirect writes terma's extension for omp pointed straight at the
// receiver, as the direct half of a comparison.
func (sb *Sandbox) UseOmpExtensionDirect() {
	sb.T.Helper()
	sb.writePiFamilyExtension(filepath.Join(sb.Home, ".omp", "agent", "extensions", "terma-relay.ts"), "omp", false)
}

// OmpRun runs one `omp -p` in dir.
func (sb *Sandbox) OmpRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "-p", prompt, "--model", "fake/m")
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("omp -p: %v\n%s", err, out.String())
	}
	return out.String()
}
