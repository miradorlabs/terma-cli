package selfupdate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// executable writes an empty executable at path, creating its directory.
func executable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestManagedByUsesThePackageManagerThatOwnsTheBinary(t *testing.T) {
	t.Setenv("PATH", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brew := filepath.Join(root, "brew")
	executable(t, filepath.Join(brew, "bin", "brew"))
	npm := filepath.Join(root, "node")
	executable(t, filepath.Join(npm, "bin", "npm"))

	for _, tc := range []struct {
		name, exe string
		manager   string
		argv      []string
		terma     string
	}{
		{"cask", filepath.Join(brew, "Caskroom", "terma", "1.0.0", "terma"), "Homebrew",
			[]string{filepath.Join(brew, "bin", "brew"), "upgrade", "--cask", "terma"}, filepath.Join(brew, "bin", "terma")},
		{"formula", filepath.Join(brew, "Cellar", "terma", "1.0.0", "bin", "terma"), "Homebrew",
			[]string{filepath.Join(brew, "bin", "brew"), "upgrade", "terma"}, filepath.Join(brew, "bin", "terma")},
		{"brew not found", filepath.Join(root, "elsewhere", "Caskroom", "terma", "1.0.0", "terma"), "Homebrew", nil, filepath.Join(root, "elsewhere", "bin", "terma")},
		{"npm global", filepath.Join(npm, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "npm",
			[]string{filepath.Join(npm, "bin", "npm"), "install", "--global", "--prefix", npm, "@miradorlabs/terma@latest"},
			filepath.Join(npm, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma")},
		// A custom prefix keeps no npm of its own, and PATH has none here either.
		{"npm prefix without npm", filepath.Join(root, "custom", "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "npm", nil,
			filepath.Join(root, "custom", "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma")},
		// A project's own dependency is that project's to upgrade.
		{"npm project", filepath.Join(root, "app", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "npm", nil,
			filepath.Join(root, "app", "node_modules", "@miradorlabs", "terma", "vendor", "terma")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := ManagedBy(tc.exe)
			if !ok || m.Name != tc.manager || !slices.Equal(m.Argv, tc.argv) || m.Terma != tc.terma || m.Command == "" {
				t.Fatalf("ManagedBy = %+v, %v", m, ok)
			}
		})
	}
	if m, ok := ManagedBy(filepath.Join(root, "bin", "terma")); ok {
		t.Fatalf("an install script's binary is not managed: %+v", m)
	}
}

// The npm on PATH is used only when the prefix has none, and always with that prefix.
func TestManagedByFallsBackToNpmOnPathForItsPrefix(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "path")
	executable(t, filepath.Join(bin, "npm"))
	t.Setenv("PATH", bin)
	prefix := filepath.Join(root, "custom")
	m, ok := ManagedBy(filepath.Join(prefix, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"))
	want := []string{filepath.Join(bin, "npm"), "install", "--global", "--prefix", prefix, "@miradorlabs/terma@latest"}
	if !ok || !slices.Equal(m.Argv, want) {
		t.Fatalf("ManagedBy = %+v, %v", m, ok)
	}
}

func TestRefreshRunsOnceForEachNewerRelease(t *testing.T) {
	dir := t.TempDir()
	if !NeedsRefresh(dir, "1.2.0") {
		t.Fatal("a machine never refreshed needs it")
	}
	if NeedsRefresh(dir, "dev") || NeedsRefresh(dir, "v0.0.2-11-g08c5516") {
		t.Fatal("a build that is not a release refreshes only when asked")
	}
	if err := SaveRefreshed(dir, "1.2.0"); err != nil {
		t.Fatal(err)
	}
	for version, want := range map[string]bool{"1.2.0": false, "1.1.9": false, "1.2.1": true, "2.0.0": true} {
		if got := NeedsRefresh(dir, version); got != want {
			t.Errorf("NeedsRefresh(%s) after 1.2.0 = %v, want %v", version, got, want)
		}
	}
	if err := SaveRefreshed(dir, "dev"); err != nil {
		t.Fatal(err)
	}
	if NeedsRefresh(dir, "1.2.0") {
		t.Fatal("a source build's refresh overwrote the release record")
	}
}
