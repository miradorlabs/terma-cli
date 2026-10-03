// Command benchhooks writes the git hooks `terma setup` writes, through the same code, for
// scripts/bench-hook.sh: TERMA_CONFIG_DIR and GIT_CONFIG_GLOBAL say where, and the one
// argument is the terma they run.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/miradorlabs/terma-cli/internal/globalmode"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchhooks <terma>")
		os.Exit(2)
	}
	m := globalmode.Machine{Terma: func() (string, error) { return os.Args[1], nil }}
	if _, err := m.ApplyGitHooks(context.Background(), true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
