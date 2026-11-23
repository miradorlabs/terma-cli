package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
)

// ompInstalled puts an OMP 19.1.0 in bin, first on PATH.
func ompInstalled(t *testing.T, bin string) {
	t.Helper()
	omp, text := filepath.Join(bin, "omp"), "#!/bin/sh\necho omp/19.1.0\n"
	if runtime.GOOS == "windows" {
		omp, text = omp+".bat", "@echo omp/19.1.0\r\n"
	}
	if err := os.WriteFile(omp, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// omp's exporter is its .env: doctor passes while its keys point at the relay, and fails
// once one sends elsewhere, or a shell export omp reads ahead of the .env does.
func TestRelayCheckReadsOmpsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	ompInstalled(t, t.TempDir())
	reg := builtin.Agents(filepath.Join(dir, "terma"))
	a, _ := reg.Find[agents.RelayExporter]("omp")
	if _, err := a.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: []string{"terma", "hook"}}); err != nil {
		t.Fatal(err)
	}
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Pass {
		t.Fatalf("RelayCheck = %+v, want a pass", c)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example")
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Pass {
		t.Fatalf("RelayCheck with a shell common endpoint = %+v, want a pass: the .env's own per signal outrank it", c)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://collector.example/v1/traces")
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Fail || !strings.Contains(c.Detail, "OMP not exporting to the local relay") {
		t.Fatalf("RelayCheck with a shell traces endpoint = %+v, want a failure naming omp", c)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	env := filepath.Join(dir, ".env")
	data, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(data), "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:43180/tok/v1/traces", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://collector.example/v1/traces", 1)
	if err := os.WriteFile(env, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Fail || !strings.Contains(c.Detail, "OMP not exporting to the local relay") {
		t.Fatalf("RelayCheck = %+v, want a failure naming omp", c)
	}
	if err := os.WriteFile(env, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "extensions", "terma-omp.ts")); err != nil {
		t.Fatal(err)
	}
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Fail || !strings.Contains(c.Detail, "terma-omp.ts") || c.Fix != "terma setup" {
		t.Fatalf("RelayCheck without terma's extension = %+v, want a failure naming it", c)
	}
}

// With no saved selection doctor judges every supported agent, OMP among them, but an OMP
// that is not installed has no exporter to judge: a machine set up before OMP was
// supported still passes, and one with OMP installed and not pointed still fails.
func TestRelayCheckWithoutASelectionJudgesOmpOnlyWhereInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(dir, ".omp", "agent"))
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	reg := builtin.Agents(filepath.Join(dir, "terma"))
	for _, name := range []string{"claude", "codex"} {
		a, _ := reg.Find[agents.RelayExporter](name)
		if _, err := a.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", HookCommand: []string{"terma", "hook"}}); err != nil {
			t.Fatal(err)
		}
	}
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", nil, "terma setup"); c.Status != Pass {
		t.Fatalf("RelayCheck without OMP installed = %+v, want a pass", c)
	}
	ompInstalled(t, bin)
	if c := RelayCheck(reg, healthy(), keyed, "proj_x", "dev", nil, "terma setup"); c.Status != Fail || !strings.Contains(c.Detail, "OMP not exporting to the local relay") {
		t.Fatalf("RelayCheck with OMP installed and not pointed = %+v, want a failure naming OMP", c)
	}
}

// The relay refuses an export that presents any token but its own, and OMP sends each signal
// to its own endpoint as given: doctor fails an OMP whose .env carries another token, or a
// traces endpoint without the traces path, from the .env or the shell, and never shows the token.
func TestRelayCheckFailsOmpsWrongTokenOrPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	ompInstalled(t, t.TempDir())
	reg := builtin.Agents(filepath.Join(dir, "terma"))
	a, _ := reg.Find[agents.RelayExporter]("omp")
	if _, err := a.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok-current", HookCommand: []string{"terma", "hook"}}); err != nil {
		t.Fatal(err)
	}
	relay := healthy()
	relay.Token = "tok-current"
	if c := RelayCheck(reg, relay, keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Pass {
		t.Fatalf("RelayCheck with the relay's token = %+v, want a pass", c)
	}
	relay.Token = "tok-rotated"
	c := RelayCheck(reg, relay, keyed, "proj_x", "dev", []string{"omp"}, "terma setup")
	if c.Status != Fail || !strings.Contains(c.Detail, "OMP not exporting to the local relay") || !strings.Contains(c.Detail, "relay token") {
		t.Fatalf("RelayCheck with another token = %+v, want a failure naming the token", c)
	}
	if strings.Contains(c.Detail+c.Fix, "tok-") {
		t.Errorf("doctor shows a token: %+v", c)
	}
	// Before setup made a token there is none to compare: the check is skipped, not failed.
	relay.Token = ""
	if c := RelayCheck(reg, relay, keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Pass {
		t.Fatalf("RelayCheck with no relay token = %+v, want a pass", c)
	}
	relay.Token = "tok-current"

	env := filepath.Join(dir, ".env")
	data, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://127.0.0.1:43180/tok-current", "http://127.0.0.1:43180/tok-current/v1/logs", "http://127.0.0.1:43180/tok-current/v1/traces/"} {
		edited := strings.Replace(string(data), "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:43180/tok-current/v1/traces", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="+bad, 1)
		if edited == string(data) {
			t.Fatal("the .env names no traces endpoint to edit")
		}
		if err := os.WriteFile(env, []byte(edited), 0o600); err != nil {
			t.Fatal(err)
		}
		if c := RelayCheck(reg, relay, keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Fail {
			t.Errorf("RelayCheck with the .env's traces endpoint %s = %+v, want a failure", bad, c)
		}
		if err := os.WriteFile(env, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", bad)
		if c := RelayCheck(reg, relay, keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Fail {
			t.Errorf("RelayCheck with the shell's traces endpoint %s = %+v, want a failure", bad, c)
		}
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://127.0.0.1:43180/tok-current")
	if c := RelayCheck(reg, relay, keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Fail {
		t.Errorf("RelayCheck with the shell's logs endpoint without /v1/logs = %+v, want a failure", c)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:43180/tok-current/v1/traces")
	if c := RelayCheck(reg, relay, keyed, "proj_x", "dev", []string{"omp"}, "terma setup"); c.Status != Pass {
		t.Fatalf("RelayCheck with the shell naming the relay's own traces URL = %+v, want a pass", c)
	}
}
