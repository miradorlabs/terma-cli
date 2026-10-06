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
	return Manager{Name: "ai.terma.relay.test", Exe: "/opt/terma/bin/terma", RelayDir: t.TempDir(),
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
	t.Parallel()
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

	earlier := earlierCommand(t, def)
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

// earlierCommand is def as an earlier terma wrote it: the relay started with a command
// this build no longer has, in each platform's own syntax.
func earlierCommand(t *testing.T, def string) string {
	t.Helper()
	for _, cmd := range [][2]string{
		{"<string>relay</string><string>run</string>", "<string>relay</string><string>serve</string>"}, // launchd
		{" relay run ", " relay serve "},     // systemd
		{" relay supervise", " relay serve"}, // the Windows launcher
	} {
		if strings.Contains(def, cmd[0]) {
			return strings.Replace(def, cmd[0], cmd[1], 1)
		}
	}
	t.Fatalf("the definition starts no relay command this test knows:\n%s", def)
	return ""
}

// A definition for another path to this same binary is current: setup run as ./bin/terma and
// install run through a symlink on PATH must not rewrite the service and restart the relay.
// A path to another binary, with identical bytes, is not.
func TestADefinitionForAnotherPathToThisBinaryIsCurrent(t *testing.T) {
	if !Supported() {
		t.Skip("no relay service on this platform")
	}
	sandboxHome(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "bin", "terma")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("terma"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Characters each renderer escapes (XML, Go quoting) that Windows still allows in a name.
	link := filepath.Join(dir, "it's & linked", "terma")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	copied := filepath.Join(dir, "copy", "terma")
	if err := os.MkdirAll(filepath.Dir(copied), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, []byte("terma"), 0o755); err != nil {
		t.Fatal(err)
	}

	installed := testManager(t)
	installed.Exe = link
	def, err := installed.Definition()
	if err != nil {
		t.Fatal(err)
	}
	writeDefinition(t, installed, def)

	viaReal := installed
	viaReal.Exe = target
	if !viaReal.Current() {
		t.Fatal("the service for a symlink to this binary reads as stale, so install would restart the relay")
	}
	other := installed
	other.Exe = copied
	if other.Current() {
		t.Fatal("the service for another binary reads as current")
	}
	otherEnv := viaReal
	otherEnv.Env = map[string]string{"HOME": "/home/dev"}
	if otherEnv.Current() {
		t.Fatal("the same binary in another environment reads as current")
	}
}

// Start starts only a service that is installed; it never writes a definition.
func TestStartNeedsAnInstalledService(t *testing.T) {
	if !Supported() {
		t.Skip("no relay service on this platform")
	}
	sandboxHome(t)
	m := testManager(t)
	if err := m.Start(t.Context()); err == nil {
		t.Fatal("Start of a service that is not installed succeeded")
	}
	if _, ok := m.Installed(); ok {
		t.Fatal("Start wrote a definition")
	}
}
