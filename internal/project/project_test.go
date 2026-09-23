package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := Load(root); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	in := &File{
		Project: Project{ID: "proj-1", Name: "Terma Frontend", OrganizationID: "org-1"},
		Install: Install{HookManager: "husky", Hooks: []string{"prepare-commit-msg", "post-commit"}, Adapters: []string{"claude"}, Version: "1.0.0", InstalledAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},
	}
	if err := Save(root, in); err != nil {
		t.Fatal(err)
	}
	// The binding lives inside .terma/, as JSON.
	if filepath.Base(Path(root)) != SettingsName || filepath.Base(filepath.Dir(Path(root))) != Dir {
		t.Fatalf("unexpected path %q", Path(root))
	}
	data, _ := os.ReadFile(Path(root))
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("settings file is not JSON: %v\n%s", err, data)
	}
	if strings.Contains(string(data), "environment") {
		t.Fatal("empty environment should be omitted (production is the default)")
	}
	out, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if out.Project != in.Project || out.Install.HookManager != "husky" || len(out.Install.Hooks) != 2 || out.Install.Adapters[0] != "claude" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if err := Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root); err != nil {
		t.Fatalf("removing twice must be fine: %v", err)
	}
}

func TestLoadRejectsMissingID(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, []byte(`{"project":{"name":"x"}}`))
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "no project id") {
		t.Fatalf("expected a missing-id error, got %v", err)
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := Save(root, &File{Project: Project{ID: "p"}}); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Find(nested)
	if err != nil || got != root {
		t.Fatalf("got %q (%v), want %q", got, err, root)
	}
	if _, err := Find(t.TempDir()); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// A repository that still carries the pre-migration .terma.toml is read as-is, and
// the next Save moves it to .terma/settings.json and deletes the legacy file.
func TestLegacyTOMLReadAndMigrate(t *testing.T) {
	root := t.TempDir()
	legacy := "[project]\nid = 'proj-legacy'\nname = 'Old'\n\n[install]\nhook_manager = 'git'\nadapters = ['claude']\n"
	if err := os.WriteFile(LegacyPath(root), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := Load(root)
	if err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	if out.Project.ID != "proj-legacy" || out.Install.HookManager != "git" || out.Install.Adapters[0] != "claude" {
		t.Fatalf("legacy parse mismatch: %+v", out)
	}
	// Find locates a repo that only has the legacy file.
	if got, err := Find(root); err != nil || got != root {
		t.Fatalf("Find legacy: got %q (%v)", got, err)
	}
	// Saving migrates: new file appears, legacy file goes away.
	if err := Save(root, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(root)); err != nil {
		t.Fatalf("new file missing after migration: %v", err)
	}
	if _, err := os.Stat(LegacyPath(root)); !os.IsNotExist(err) {
		t.Fatalf("legacy file should be removed after migration, stat err = %v", err)
	}
}

// Removing the binding must not take the committed hook shims under .terma/hooks/
// down with it.
func TestRemoveKeepsHooksDir(t *testing.T) {
	root := t.TempDir()
	if err := Save(root, &File{Project: Project{ID: "p"}}); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(root, Dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hooks, "post-commit")); err != nil {
		t.Fatalf("hook shim was removed with the binding: %v", err)
	}
	if _, err := os.Stat(Path(root)); !os.IsNotExist(err) {
		t.Fatalf("binding should be gone, stat err = %v", err)
	}
}
