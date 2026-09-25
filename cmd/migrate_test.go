package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// oldRoutingRecord writes the record 0.0.2 wrote for a developer routing Codex: no cli
// field, which today's router reads as "do not route the Codex CLI".
func oldRoutingRecord(t *testing.T) string {
	t.Helper()
	dir, err := shim.RoutingDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, testProjectID+".json")
	body := `{"project_id":"` + testProjectID + `","endpoint":"https://otel","signals":["logs"],"include_prompts":true,"include_tool_content":true,"harnesses":["codex"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func routesCodexCLI(t *testing.T) bool {
	t.Helper()
	rec, ok, err := shim.LoadRecord(testProjectID)
	if err != nil || !ok {
		t.Fatalf("LoadRecord: %v %v", ok, err)
	}
	return rec.CLI
}

// The first start of a new build migrates, whatever the command — here a hook, which
// must stay silent — and records it so no later start does it again.
func TestEveryStartMigratesSavedStateOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	oldRoutingRecord(t)
	if routesCodexCLI(t) {
		t.Fatal("precondition: an old record does not route the Codex CLI")
	}
	migrateState(context.Background(), []string{"hook", "post-tool-use"})
	if !routesCodexCLI(t) {
		t.Fatal("a hook start did not migrate the routing record")
	}
	if s, _ := migrate.Load(dir); s.Applied != migrate.Latest() || migrate.Pending(dir) {
		t.Fatalf("state %+v", s)
	}
}

// update --refresh migrates first and says so.
func TestRefreshMigratesSavedState(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	oldRoutingRecord(t)
	out, err := runTerma(t, "update", "--refresh")
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Migrated saved state: route Codex CLI") || !routesCodexCLI(t) {
		t.Fatalf("output:\n%s", out)
	}
	if out, _ := runTerma(t, "update", "--refresh"); strings.Contains(out, "Migrated") {
		t.Fatalf("a second refresh migrated again:\n%s", out)
	}
}

// doctor has a line for saved state only when an update left it unmigrated.
func TestDoctorReportsUnmigratedState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	record := func(s migrate.State) {
		t.Helper()
		data, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(dir, "migrations.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record(migrate.State{Applied: migrate.Latest()})
	if c, ok := stateCheck(); ok {
		t.Fatalf("a migrated machine got a line: %+v", c)
	}
	record(migrate.State{Applied: 0, Failed: &migrate.Failure{ID: 1, Name: "the change", At: time.Now(), Error: "permission denied"}})
	if c, ok := stateCheck(); !ok || c.Status != doctor.Fail || !strings.Contains(c.Detail, "permission denied") || c.Fix != "terma update --refresh" {
		t.Fatalf("failed migration: %+v %v", c, ok)
	}
	record(migrate.State{Applied: 0})
	if c, ok := stateCheck(); !ok || c.Status != doctor.Warn || c.Fix != "terma update --refresh" {
		t.Fatalf("pending migration: %+v %v", c, ok)
	}
	if err := config.WriteJSON(filepath.Join(dir, "migrations.json"), migrate.State{Applied: migrate.Latest() + 5}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := stateCheck(); ok {
		t.Fatal("a newer build's record is not this build's to report")
	}
}
