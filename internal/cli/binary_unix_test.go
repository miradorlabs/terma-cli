//go:build unix

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	termaOnce sync.Once
	termaPath string
	termaErr  error
)

// termaBinary builds terma once per test run, for tests that run it as hooks do.
func termaBinary(t *testing.T) string {
	t.Helper()
	termaOnce.Do(func() {
		dir, err := os.MkdirTemp("", "terma-e2e-bin")
		if err != nil {
			termaErr = err
			return
		}
		termaPath = filepath.Join(dir, "terma")
		out, err := exec.Command("go", "build", "-o", termaPath, "github.com/miradorlabs/terma-cli/cmd/terma").CombinedOutput()
		if err != nil {
			termaErr = fmt.Errorf("build terma: %w\n%s", err, out)
		}
	})
	if termaErr != nil {
		t.Fatal(termaErr)
	}
	return termaPath
}
