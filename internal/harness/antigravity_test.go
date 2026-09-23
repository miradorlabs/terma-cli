package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAntigravityTrustsWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resolved, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	a := Antigravity{}

	// No settings file yet: nothing trusted, and not an error.
	if ok, err := a.TrustsWorkspace(resolved); err != nil || ok {
		t.Fatalf("fresh machine: ok=%v err=%v", ok, err)
	}
	settings := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"trustedWorkspaces": ["`+link+`/"], "colorScheme": "terminal"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{resolved, link, link + "/."} {
		if ok, err := a.TrustsWorkspace(root); err != nil || !ok {
			t.Errorf("%s: trusted through the recorded symlink should read as trusted (ok=%v err=%v)", root, ok, err)
		}
	}
	if ok, _ := a.TrustsWorkspace(t.TempDir()); ok {
		t.Error("an unrelated directory read as trusted")
	}
	if err := os.WriteFile(settings, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TrustsWorkspace(resolved); err == nil {
		t.Error("a settings file terma cannot parse must be reported, not read as untrusted")
	}
}

func TestAntigravityIsNotATelemetryHarness(t *testing.T) {
	if _, err := Lookup("antigravity"); err == nil {
		t.Fatal("agy has no OTLP exporter and must not be connectable")
	}
	if a, ok := LookupSupport("antigravity"); !ok || a.Telemetry.Level != SupportPartial || a.Attribution.Level != SupportFull || a.Support != SupportPartial {
		t.Fatalf("support catalog entry wrong: %+v", a)
	}
	// Detect never fails; whether agy is installed is the machine's business.
	_ = Antigravity{}.Detect(context.Background())
}
