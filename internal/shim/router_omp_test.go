package shim

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// omp is pointed at the local relay by its environment, in a bound repository, on a
// machine where the relay is set up — and nowhere else, so omp elsewhere exports nothing.
func TestOmpRoutesToTheRelayOnlyInABoundRepository(t *testing.T) {
	sandbox(t)
	cfgDir, _ := config.Dir()
	relay := filepath.Join(cfgDir, "relay")
	if err := os.MkdirAll(relay, 0o700); err != nil {
		t.Fatal(err)
	}
	bound := t.TempDir()
	if err := termaproject.Save(bound, &termaproject.File{Project: termaproject.Project{ID: "proj_omp"}}); err != nil {
		t.Fatal(err)
	}
	if r := routeFor(AgentOmp, bound, nil); len(r.env) != 0 {
		t.Fatalf("no relay set up, but omp was routed: %v", r.env)
	}
	_ = os.WriteFile(filepath.Join(relay, "token"), []byte("tok\n"), 0o600)
	_ = os.WriteFile(filepath.Join(relay, "addr"), []byte("127.0.0.1:5555\n"), 0o600)
	r := routeFor(AgentOmp, bound, nil)
	if r.env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:5555" || r.env["OTEL_EXPORTER_OTLP_HEADERS"] != "Authorization=Bearer tok" || r.env["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" {
		t.Fatalf("env = %v", r.env)
	}
	if r := routeFor(AgentOmp, t.TempDir(), nil); len(r.env) != 0 {
		t.Fatalf("omp outside a bound repository was routed: %v", r.env)
	}
	plan := t.TempDir()
	t.Chdir(bound)
	if err := Prepare(AgentOmp, plan, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(plan, "envcount")); string(data) != "terma-env-v1:3\n" {
		t.Fatalf("envcount = %q", data)
	}
}
