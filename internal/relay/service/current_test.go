package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxHome puts the per-user service directories (LaunchAgents, systemd --user) under a
// temporary home, so a test never reads or writes the machine's own.
func sandboxHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func testManager(t *testing.T) Manager {
	t.Helper()
	return Manager{Name: "ai.terma.relay.test", Exe: "/opt/terma/bin/terma", StateDir: t.TempDir(),
		Env: map[string]string{"HOME": "/home/dev", "TERMA_ENV": "dev"}}
}

func writeDefinition(t *testing.T, m Manager, content string) {
	t.Helper()
	path, err := m.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Definition is what Install writes: the platform's renderer over the manager's binary,
// environment and state directory.
func TestDefinitionIsWhatInstallWrites(t *testing.T) {
	if !Supported() {
		t.Skip("no relay service on this platform")
	}
	m := testManager(t)
	def, err := m.Definition()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{m.Exe, "relay", "TERMA_ENV"} {
		if !strings.Contains(def, want) {
			t.Errorf("definition lacks %q:\n%s", want, def)
		}
	}
	if again, _ := m.Definition(); again != def {
		t.Fatal("the definition is not deterministic, so an unchanged service would read as stale")
	}
}

// Only the definition this terma would write now is current: a missing one, one an earlier
// terma wrote (a command this build lacks), one for another binary, or one for another
// environment is not.
func TestOnlyTheDefinitionThisTermaWritesIsCurrent(t *testing.T) {
	if !Supported() {
		t.Skip("no relay service on this platform")
	}
	sandboxHome(t)
	m := testManager(t)
	if m.Current() {
		t.Fatal("a missing definition reads as current")
	}
	def, err := m.Definition()
	if err != nil {
		t.Fatal(err)
	}
	writeDefinition(t, m, def)
	if !m.Current() {
		t.Fatal("the definition Install writes does not read as current")
	}

	earlier := strings.Replace(def, "run", "serve", 1)
	if earlier == def {
		t.Fatal("the definition names no `relay run` to replace")
	}
	writeDefinition(t, m, earlier)
	if m.Current() {
		t.Fatal("an earlier terma's definition, running a removed command, reads as current")
	}

	writeDefinition(t, m, def)
	moved := m
	moved.Exe = "/usr/local/bin/terma"
	if moved.Current() {
		t.Fatal("a definition for another binary reads as current")
	}

	prod := m
	prod.Env = map[string]string{"HOME": "/home/dev"}
	if prod.Current() {
		t.Fatal("a definition for another environment reads as current: the relay would deliver to the wrong backend")
	}
	if !m.Current() {
		t.Fatal("checking other managers changed the installed definition")
	}
}
