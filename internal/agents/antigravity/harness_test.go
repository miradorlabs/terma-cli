package antigravity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrustsWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resolved, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Skip("symlinks unavailable")
	}

	// No settings file yet: nothing trusted, and not an error.
	if ok, err := trustsWorkspace(resolved); err != nil || ok {
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
		if ok, err := trustsWorkspace(root); err != nil || !ok {
			t.Errorf("%s: trusted through the recorded symlink should read as trusted (ok=%v err=%v)", root, ok, err)
		}
	}
	if ok, _ := trustsWorkspace(t.TempDir()); ok {
		t.Error("an unrelated directory read as trusted")
	}
	if err := os.WriteFile(settings, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := trustsWorkspace(resolved); err == nil {
		t.Error("a settings file terma cannot parse must be reported, not read as untrusted")
	}
}
