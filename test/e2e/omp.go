package e2e

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// OMP exports natively, reading OTEL_* privately from ~/.omp/agent/.env, which
// relay setup points at the relay; terma's extension
// (internal/agents/omp/terma-omp.ts, ~/.omp/agent/extensions/terma-omp.ts)
// exports nothing and only hands sessions, prompts and file edits to
// `terma hook omp-*`, so its commits carry attribution. The direct half of a comparison
// points the native exporter at the receiver. The builds are the installed one, or
// TERMA_E2E_OMP_VERSIONS's: each release's standalone binary from GitHub, checked against
// the release's SHA256SUMS.txt (the npm package is over a gigabyte).

// ompPackage is OMP's npm package, which lists its releases.
const ompPackage = "@oh-my-pi/pi-coding-agent"

func forEachOmp(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	skipUnlessSelected(t, "omp")
	spec := cmp.Or(os.Getenv("TERMA_E2E_OMP_VERSIONS"), "installed")
	builds := resolve(t, "omp", spec, func() ([]string, error) { return npmReleases(ompPackage) }, ensureOmp)
	if len(builds) == 0 {
		Record(t.Name(), "not run", "omp is not installed")
		t.Skip("omp is not installed")
	}
	for _, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { scenarioOfBuild(t, b); run(t, b) })
	}
}

// ensureOmp fetches the release's binary for this platform, checked against its SHA256SUMS.txt.
func ensureOmp(version string) (string, error) {
	if err := CheckPathName("omp version", version); err != nil {
		return "", err
	}
	path := filepath.Join(versionsDir(), "omp", version, "omp")
	if _, err := os.Stat(path); err == nil {
		return abs(path), nil
	}
	asset := "omp-" + runtime.GOOS + "-" + map[string]string{"arm64": "arm64", "amd64": "x64"}[runtime.GOARCH]
	base := "https://github.com/can1357/oh-my-pi/releases/download/v" + version + "/"
	sums, err := fetch(base + "SHA256SUMS.txt")
	if err != nil {
		return "", err
	}
	want := ""
	for line := range strings.Lines(string(sums)) {
		if sum, name, ok := strings.Cut(strings.TrimSpace(line), "  "); ok && name == asset {
			want = sum
		}
	}
	if want == "" {
		return "", fmt.Errorf("omp %s: no %s in SHA256SUMS.txt", version, asset)
	}
	bin, err := fetch(base + asset)
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(bin); hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("omp %s: checksum mismatch", version)
	}
	if err := writeBuild(path, bin); err != nil {
		return "", err
	}
	return abs(path), nil
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
