package live

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// omp (oh-my-pi) exports OTLP natively, configured only by OTEL_* variables; terma
// claims its sessions through the committed hook file `terma install --adapters omp`
// writes, and, on a relay machine, hands it the relay's variables through its PATH
// shim. Only the installed build is tested: the npm package is over a gigabyte.

func forEachOmp(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
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

// OmpRun runs one `omp -p` in dir. Relayed, it starts through terma's shim, as it
// would from a shell with the shim directory on PATH; otherwise straight, exporting
// to the receiver through the OTEL_* variables omp reads.
func (sb *Sandbox) OmpRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	launcher := b.Path
	env := sb.termaEnv()
	if sb.relayed {
		launcher = filepath.Join(sb.TermaConfig, "shim", "bin", "omp")
	} else {
		env = append(env, "OTEL_EXPORTER_OTLP_ENDPOINT="+sb.Receiver.URL(), "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
			"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer "+liveKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "-p", prompt, "--model", "fake/m")
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("omp -p: %v\n%s", err, out.String())
	}
	return out.String()
}
