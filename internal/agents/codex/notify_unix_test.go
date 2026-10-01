//go:build unix

package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The notify edit writes through a symlinked config.toml, as Connect does, instead of
// renaming over the link.
func TestCodexNotifyWritesThroughSymlinkAndKeepsTheMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	target := filepath.Join(t.TempDir(), "dotfiles", "codex.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("model = \"gpt-5.4\"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name string
		run  func() (bool, error)
		want bool
	}{
		{"install", func() (bool, error) { return exporter{}.installNotify() }, true},
		{"remove", func() (bool, error) { return exporter{}.removeNotify() }, false},
	} {
		if _, err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s replaced the symlink with a regular file", step.name)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(data), "codex-notify"); got != step.want {
			t.Fatalf("%s: the link's target has terma's notify = %v, want %v:\n%s", step.name, got, step.want, data)
		}
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o400 {
			t.Fatalf("%s loosened a 0400 file to %v", step.name, info.Mode().Perm())
		}
	}
}

// Concurrent connects under different CODEX_HOMEs keep both notifiers.
func TestConcurrentNotifyChainsKeepEveryConfig(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const configs = 16

	var wg sync.WaitGroup
	for i := range configs {
		wg.Go(func() {
			path := fmt.Sprintf("/home/dev/codex-%02d/config.toml", i)
			if err := saveCodexNotifyChain(path, []string{fmt.Sprintf("notifier-%02d", i)}); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	rec, _, err := loadCodexNotifyRecord()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Chains) != configs {
		t.Fatalf("the record kept %d of %d chains", len(rec.Chains), configs)
	}
}
