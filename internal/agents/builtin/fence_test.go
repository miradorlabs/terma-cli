package builtin

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// reachFailedCommand is the shell reach an agent's plugin sends for a session it dropped
// after a failed reach check: terma cannot follow it, so the hook marks the run Mixed.
const reachFailedCommand = "eval $TERMA_REACH_FAILED"

// The OpenCode plugin and OMP's and Pi's extensions ask terma to mark a dropped session's run Mixed
// with the same command.
func TestPluginsSendOneReachFailedCommand(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		filepath.Join("..", "opencode", "plugin", "terma.js"): "const REACH_FAILED_COMMAND = " + strconv.Quote(reachFailedCommand) + "\n",
		filepath.Join("..", "omp", "terma-omp.ts"):            "command: " + strconv.Quote(reachFailedCommand) + ",",
		filepath.Join("..", "pi", "terma.ts"):                 "const REACH_FAILED_COMMAND = " + strconv.Quote(reachFailedCommand) + "\n",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not send %q: want a line with %s", path, reachFailedCommand, want)
		}
	}
}
