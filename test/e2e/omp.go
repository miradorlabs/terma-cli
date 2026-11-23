package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// OMP exports natively, reading OTEL_* privately from ~/.omp/agent/.env, which
// relay setup points at the relay; terma's extension
// (internal/agents/omp/terma-omp.ts, ~/.omp/agent/extensions/terma-omp.ts)
// exports nothing and only hands sessions, prompts and file edits to
// `terma hook omp-*`, so its commits carry attribution. The direct half of a comparison
// points the native exporter at the receiver. Only the installed build is tested: the npm
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
	t.Run(b.Label(), func(t *testing.T) { scenarioOf(t, b.Harness); run(t, b) })
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

// UseOmpNativeDirect points omp's native exporter straight at the receiver, as the
// direct half of a comparison.
func (sb *Sandbox) UseOmpNativeDirect() {
	sb.writeAbs(filepath.Join(sb.Home, ".omp", "agent", ".env"), "OTEL_EXPORTER_OTLP_ENDPOINT="+sb.Receiver.URL()+`
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20`+liveKey+`
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=full
`)
}

// OmpRun runs one `omp -p` in dir, with any further flags.
func (sb *Sandbox) OmpRun(b Binary, dir, prompt string, flags ...string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, append([]string{"-p", prompt, "--model", "fake/m"}, flags...)...)
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("omp -p: %v\n%s", err, out.String())
	}
	return out.String()
}
