//go:build unix

package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Removing terma's notify writes through a symlinked config.toml instead of renaming over
// the link, and keeps the file's mode.
func TestCodexNotifyWritesThroughSymlinkAndKeepsTheMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configDir := t.TempDir()

	target := filepath.Join(t.TempDir(), "dotfiles", "codex.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(termaNotifyLine+"\nmodel = \"gpt-5.4\"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (exporter{dir: configDir}).removeNotify(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("remove replaced the symlink with a regular file")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "model = \"gpt-5.4\"\n" {
		t.Fatalf("the link's target is %q (%v), want terma's notify gone", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("remove loosened a 0400 file to %v", info.Mode().Perm())
	}
}

// Concurrent removals under different CODEX_HOMEs each clear their own chain and keep the rest.
func TestConcurrentNotifyClearsKeepEveryOtherConfig(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	const configs = 16
	path := func(i int) string { return fmt.Sprintf("/home/dev/codex-%02d/config.toml", i) }
	for i := range 2 * configs {
		recordDisplaced(t, configDir, path(i), []string{fmt.Sprintf("notifier-%02d", i)})
	}

	var wg sync.WaitGroup
	for i := range configs {
		wg.Go(func() {
			if err := clearCodexNotifyChain(configDir, path(i)); err != nil {
				t.Errorf("clear %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	rec, _, err := loadCodexNotifyRecord(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Chains) != configs {
		t.Fatalf("the record kept %d chains, want the %d not cleared", len(rec.Chains), configs)
	}
}
