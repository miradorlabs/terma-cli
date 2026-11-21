package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
)

// omp's exporter is its .env: doctor passes while its keys point at the relay, and fails
// once one sends elsewhere.
func TestRelayCheckReadsOmpsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	reg := builtin.Agents(filepath.Join(dir, "terma"))
	a, _ := reg.Find[agents.RelayExporter]("omp")
	if _, err := a.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: []string{"terma", "hook"}}); err != nil {
		t.Fatal(err)
	}
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", []string{"omp"}); c.Status != Pass {
		t.Fatalf("RelayCheck = %+v, want a pass", c)
	}
	env := filepath.Join(dir, ".env")
	data, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(data), "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:43180/tok/v1/traces", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://collector.example/v1/traces", 1)
	if err := os.WriteFile(env, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", []string{"omp"}); c.Status != Fail || !strings.Contains(c.Detail, "Omp not exporting to the local relay") {
		t.Fatalf("RelayCheck = %+v, want a failure naming omp", c)
	}
}
